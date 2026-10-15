package localmx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

func (c *Connector) createRoom(ctx context.Context, req *mautrix.ReqCreateRoom) (id.RoomID, error) {
	roomID := req.BeeperLocalRoomID
	if roomID == "" {
		return "", errors.New("room creation without a deterministic room ID")
	}
	kind := store.KindGroup
	if req.IsDirect {
		kind = store.KindDM
	} else if roomType, _ := req.CreationContent["type"].(event.RoomType); roomType == event.RoomTypeSpace {
		kind = store.KindSpace
	}
	var threadKey string
	for _, evt := range req.InitialState {
		if evt.Type == event.StateBridge {
			if info, ok := evt.Content.Parsed.(*event.BridgeEventContent); ok {
				threadKey = info.Channel.ID
			}
		}
	}
	if threadKey == "" {
		return "", errors.New("room creation without bridge info")
	}
	err := c.write(ctx, func(tx *store.Tx) error {
		conversationID, err := tx.UpsertConversation(string(roomID), threadKey, kind)
		if err != nil {
			return err
		}
		if req.PowerLevelOverride != nil {
			if err = c.applyState(ctx, tx, conversationID, roomID, BotMXID, event.StatePowerLevels, "", &event.Content{Parsed: req.PowerLevelOverride}, 0); err != nil {
				return err
			}
		}
		for _, evt := range req.InitialState {
			evtType := evt.Type
			evtType.Class = event.StateEventType
			if err = c.applyState(ctx, tx, conversationID, roomID, BotMXID, evtType, evt.GetStateKey(), &evt.Content, 0); err != nil {
				return err
			}
		}
		return nil
	})
	return roomID, err
}

func (c *Connector) sendState(ctx context.Context, sender id.UserID, roomID id.RoomID, evtType event.Type, stateKey string, content *event.Content, ts time.Time) (id.EventID, error) {
	evtType.Class = event.StateEventType
	err := c.write(ctx, func(tx *store.Tx) error {
		conversationID, _, err := tx.ConversationByRoom(string(roomID))
		if err != nil {
			return fmt.Errorf("room %s: %w", roomID, err)
		}
		return c.applyState(ctx, tx, conversationID, roomID, sender, evtType, stateKey, content, timestampMS(ts))
	})
	return RandomEventID(), err
}

// applyState records a state event and updates the conversation columns derived from it. ts is zero for state set
// while syncing, and the remote change's time otherwise; only the latter shows up in the timeline.
func (c *Connector) applyState(ctx context.Context, tx *store.Tx, conversationID int64, roomID id.RoomID, sender id.UserID, evtType event.Type, stateKey string, content *event.Content, ts int64) error {
	if content.Parsed == nil {
		if err := content.ParseRaw(evtType); err != nil && !errors.Is(err, event.ErrUnsupportedContentType) {
			return fmt.Errorf("failed to parse %s content: %w", evtType.Type, err)
		}
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return err
	}
	if err = tx.SetRoomState(string(roomID), evtType.Type, stateKey, string(sender), string(raw)); err != nil {
		return err
	}
	inTimeline := ts != 0
	if exclude, _ := content.Raw["com.beeper.exclude_from_timeline"].(bool); exclude {
		inTimeline = false
	}
	update := func(author id.UserID, msgtype, body string) error {
		if !inTimeline || author == BotMXID {
			return nil
		}
		authorID, err := c.personFor(ctx, tx, author)
		if err != nil {
			return err
		}
		return tx.PutUpdate(conversationID, authorID, string(RandomEventID()), ts, msgtype, body, store.SourceLive)
	}

	switch parsed := content.Parsed.(type) {
	case *event.RoomNameEventContent:
		if err = tx.SetConversationTitle(conversationID, parsed.Name); err != nil {
			return err
		}
		return update(sender, "name", fmt.Sprintf("named the group “%s”", parsed.Name))
	case *event.TopicEventContent:
		return tx.SetConversationTopic(conversationID, parsed.Topic)
	case *event.RoomAvatarEventContent:
		if err = tx.SetConversationAvatar(conversationID, SHAFromContentURI(parsed.URL)); err != nil {
			return err
		}
		return update(sender, "avatar", "changed the group photo")
	case *event.BeeperDisappearingTimer:
		if err = tx.SetConversationDisappearing(conversationID, string(parsed.Type), parsed.Timer.Milliseconds()); err != nil {
			return err
		}
		if parsed.Type == event.DisappearingTypeNone || parsed.Timer.Duration == 0 {
			return update(sender, "disappearing", "turned off disappearing messages")
		}
		return update(sender, "disappearing", fmt.Sprintf("set messages to disappear after %s", parsed.Timer.Duration))
	case *event.SpaceParentEventContent:
		parentID, _, err := tx.ConversationByRoom(stateKey)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		return tx.SetConversationParent(conversationID, parentID)
	case *event.SpaceChildEventContent:
		childID, _, err := tx.ConversationByRoom(stateKey)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		if len(parsed.Via) == 0 {
			return tx.SetConversationParent(childID, 0)
		}
		return tx.SetConversationParent(childID, conversationID)
	case *event.MemberEventContent:
		return c.applyMembership(ctx, tx, conversationID, roomID, sender, id.UserID(stateKey), parsed, update)
	}
	return nil
}

func (c *Connector) applyMembership(ctx context.Context, tx *store.Tx, conversationID int64, roomID id.RoomID, sender, target id.UserID, content *event.MemberEventContent, update func(author id.UserID, msgtype, body string) error) error {
	if target == BotMXID {
		return nil
	}
	personID, err := c.personFor(ctx, tx, target)
	if err != nil {
		return err
	}
	previous, err := tx.SetMembership(conversationID, personID, string(content.Membership))
	if err != nil {
		return err
	}
	_, kind, err := tx.ConversationByRoom(string(roomID))
	if err != nil {
		return err
	}
	if kind == store.KindDM && target != OwnerMXID && (content.Membership == event.MembershipJoin || content.Membership == event.MembershipInvite) {
		if ghostID, ok := ParseGhostMXID(target); ok && !c.isSelf(ghostID) {
			if err = tx.SetConversationOtherPerson(conversationID, personID); err != nil {
				return err
			}
		}
	}
	if kind != store.KindGroup {
		return nil
	}
	wasIn := previous == string(event.MembershipJoin)
	isIn := content.Membership == event.MembershipJoin
	switch {
	case wasIn == isIn:
		return nil
	case isIn:
		return update(target, "member", "joined the group")
	case sender != target && sender != BotMXID:
		targetName, err := tx.PersonName(personID)
		if err != nil {
			return err
		}
		return update(sender, "member", fmt.Sprintf("removed %s", targetName))
	default:
		return update(target, "member", "left the group")
	}
}
