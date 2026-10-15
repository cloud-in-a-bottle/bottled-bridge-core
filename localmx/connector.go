// Package localmx is the Matrix side of the bridge, without Matrix: it implements bridgev2's MatrixConnector by
// writing what the bridge would send to a homeserver into the message store instead. Rooms are conversations, ghost
// users are people, and the one bridge user (OwnerMXID) is the account owner.
package localmx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/files"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

type Connector struct {
	br     *bridgev2.Bridge
	store  *store.Store
	files  *files.Store
	broker *events.Broker
	log    zerolog.Logger

	bot   *intent
	owner *intent

	statusMu sync.Mutex
	status   status.BridgeState
}

var (
	_ bridgev2.MatrixConnector                       = (*Connector)(nil)
	_ bridgev2.MatrixConnectorWithArbitraryRoomState = (*Connector)(nil)
)

func New(st *store.Store, fs *files.Store, broker *events.Broker, log zerolog.Logger) *Connector {
	c := &Connector{store: st, files: fs, broker: broker, log: log}
	c.bot = &intent{c: c, mxid: BotMXID}
	c.owner = &intent{c: c, mxid: OwnerMXID}
	return c
}

func (c *Connector) Init(br *bridgev2.Bridge) {
	c.br = br
}

func (c *Connector) Start(ctx context.Context) error { return nil }
func (c *Connector) PreStop()                        {}
func (c *Connector) Stop()                           {}

func (c *Connector) GetCapabilities() *bridgev2.MatrixCapabilities {
	// Batch sending turns on history backfill: messages are inserted with their original timestamps.
	return &bridgev2.MatrixCapabilities{BatchSending: true}
}

func (c *Connector) ParseGhostMXID(userID id.UserID) (networkid.UserID, bool) {
	return ParseGhostMXID(userID)
}

func (c *Connector) FormatGhostMXID(userID networkid.UserID) id.UserID {
	return FormatGhostMXID(userID)
}

func (c *Connector) GhostIntent(userID networkid.UserID) bridgev2.MatrixAPI {
	return &intent{c: c, mxid: FormatGhostMXID(userID), ghost: userID}
}

// NewUserIntent is how the bridge gets a "double puppet" for the owner, which it uses for the owner's own messages,
// read markers and chat settings from other devices.
func (c *Connector) NewUserIntent(ctx context.Context, userID id.UserID, accessToken string) (bridgev2.MatrixAPI, string, error) {
	if userID != OwnerMXID {
		return nil, "", fmt.Errorf("no such user %s", userID)
	}
	return c.owner, accessToken, nil
}

func (c *Connector) BotIntent() bridgev2.MatrixAPI {
	return c.bot
}

// BridgeStatus is the latest connection state of the network login.
func (c *Connector) BridgeStatus() status.BridgeState {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return c.status
}

func (c *Connector) SendBridgeStatus(ctx context.Context, state *status.BridgeState) error {
	if state.RemoteID == "" {
		// The bridge's global state (eg UNCONFIGURED before any login), not the network connection's.
		return nil
	}
	c.statusMu.Lock()
	c.status = *state
	c.statusMu.Unlock()
	c.broker.Publish("status", state)
	return nil
}

func (c *Connector) SendMessageStatus(ctx context.Context, ms *bridgev2.MessageStatus, info *bridgev2.MessageStatusEventInfo) {
	if info == nil || info.SourceEventID == "" {
		return
	}
	var newStatus, errText, remoteID string
	switch ms.Status {
	case event.MessageStatusSuccess:
		newStatus = "sent"
		if len(ms.DeliveredTo) > 0 {
			newStatus = "delivered"
		}
		part, err := c.br.DB.Message.GetPartByMXID(ctx, info.SourceEventID)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to look up sent message's remote ID")
		} else if part != nil {
			remoteID = string(part.ID)
		}
	case event.MessageStatusPending:
		return
	default:
		newStatus = "failed"
		errText = ms.Message
		if errText == "" && ms.InternalError != nil {
			errText = ms.InternalError.Error()
		}
	}
	c.write(ctx, func(tx *store.Tx) error {
		err := tx.SetStatus(string(info.SourceEventID), newStatus, errText, remoteID)
		if errors.Is(err, store.ErrNotFound) {
			// A status for a reaction, deletion or read receipt: those aren't tracked.
			return nil
		}
		return err
	})
}

func (c *Connector) GenerateContentURI(ctx context.Context, mediaID networkid.MediaID) (id.ContentURIString, error) {
	return "", errors.New("direct media is not supported")
}

func (c *Connector) ParseContentURI(ctx context.Context, contentURI id.ContentURIString) (networkid.MediaID, error) {
	return nil, errors.New("direct media is not supported")
}

func (c *Connector) GetPowerLevels(ctx context.Context, roomID id.RoomID) (*event.PowerLevelsEventContent, error) {
	state, err := c.store.RoomState(ctx, string(roomID), event.StatePowerLevels.Type)
	if err != nil {
		return nil, err
	}
	pl := &event.PowerLevelsEventContent{}
	if entry, ok := state[""]; ok {
		if err = json.Unmarshal([]byte(entry.Content), pl); err != nil {
			return nil, err
		}
	}
	return pl, nil
}

func (c *Connector) GetMembers(ctx context.Context, roomID id.RoomID) (map[id.UserID]*event.MemberEventContent, error) {
	state, err := c.store.RoomState(ctx, string(roomID), event.StateMember.Type)
	if err != nil {
		return nil, err
	}
	members := make(map[id.UserID]*event.MemberEventContent, len(state))
	for key, entry := range state {
		var content event.MemberEventContent
		if err = json.Unmarshal([]byte(entry.Content), &content); err != nil {
			return nil, err
		}
		members[id.UserID(key)] = &content
	}
	return members, nil
}

func (c *Connector) GetMemberInfo(ctx context.Context, roomID id.RoomID, userID id.UserID) (*event.MemberEventContent, error) {
	members, err := c.GetMembers(ctx, roomID)
	if err != nil {
		return nil, err
	}
	return members[userID], nil
}

func (c *Connector) GetStateEvent(ctx context.Context, roomID id.RoomID, eventType event.Type, stateKey string) (*event.Event, error) {
	state, err := c.store.RoomState(ctx, string(roomID), eventType.Type)
	if err != nil {
		return nil, err
	}
	entry, ok := state[stateKey]
	if !ok {
		return nil, nil
	}
	evt := &event.Event{
		Type:     eventType,
		StateKey: &stateKey,
		Sender:   id.UserID(entry.Sender),
		RoomID:   roomID,
	}
	evt.Type.Class = event.StateEventType
	if err = json.Unmarshal([]byte(entry.Content), &evt.Content); err != nil {
		return nil, err
	}
	if err = evt.Content.ParseRaw(evt.Type); err != nil && !errors.Is(err, event.ErrContentAlreadyParsed) {
		return nil, err
	}
	return evt, nil
}

func (c *Connector) BatchSend(ctx context.Context, roomID id.RoomID, req *mautrix.ReqBeeperBatchSend, extras []*bridgev2.MatrixSendExtra) (*mautrix.RespBeeperBatchSend, error) {
	resp := &mautrix.RespBeeperBatchSend{EventIDs: make([]id.EventID, len(req.Events))}
	var lastTS int64
	err := c.write(ctx, func(tx *store.Tx) error {
		for i, evt := range req.Events {
			var extra *bridgev2.MatrixSendExtra
			if i < len(extras) {
				extra = extras[i]
			}
			te := timelineEvent{
				roomID:  roomID,
				eventID: evt.ID,
				evtType: evt.Type,
				sender:  evt.Sender,
				ts:      evt.Timestamp,
				content: &evt.Content,
				source:  store.SourceBackfill,
			}
			if extra != nil {
				te.meta = extra.MessageMeta
			}
			if err := c.applyTimelineEvent(ctx, tx, te); err != nil {
				return fmt.Errorf("event %d (%s): %w", i, evt.ID, err)
			}
			resp.EventIDs[i] = evt.ID
			lastTS = max(lastTS, evt.Timestamp)
		}
		if req.MarkReadBy == OwnerMXID && lastTS > 0 {
			conversationID, _, err := tx.ConversationByRoom(string(roomID))
			if err != nil {
				return err
			}
			return tx.SetLastRead(conversationID, lastTS)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Connector) GenerateDeterministicRoomID(portalKey networkid.PortalKey) id.RoomID {
	return RoomIDFor(portalKey)
}

func (c *Connector) GenerateDeterministicEventID(roomID id.RoomID, portalKey networkid.PortalKey, messageID networkid.MessageID, partID networkid.PartID) id.EventID {
	return MessageEventID(portalKey, messageID, partID)
}

func (c *Connector) GenerateReactionEventID(roomID id.RoomID, targetMessage *database.Message, sender networkid.UserID, emojiID networkid.EmojiID) id.EventID {
	if targetMessage == nil {
		return RandomEventID()
	}
	return ReactionEventID(targetMessage.Room, targetMessage.ID, targetMessage.PartID, sender, string(emojiID))
}

func (c *Connector) ServerName() string {
	return ServerName
}

// write runs fn in a store transaction, then deletes files it orphaned and tells browsers what changed.
func (c *Connector) write(ctx context.Context, fn func(tx *store.Tx) error) error {
	res, err := c.store.Tx(ctx, fn)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to write to message store")
		return err
	}
	if err = c.files.Delete(res.Orphaned); err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to delete orphaned attachment files")
	}
	for _, conversationID := range res.Touched {
		c.broker.Publish("conversation", events.ConversationChanged{ConversationID: conversationID})
	}
	return nil
}

func nowMS() int64 {
	return time.Now().UnixMilli()
}

func timestampMS(ts time.Time) int64 {
	if ts.IsZero() {
		return 0
	}
	return ts.UnixMilli()
}
