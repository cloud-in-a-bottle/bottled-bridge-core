package bridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/util/ptr"
	"go.mau.fi/util/variationselector"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/localmx"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

// ImportedMessage is a message from an export of the network's history that has the network's own message IDs (eg
// a backup from a phone), converted into parts as the connector converts live ones. Importing stores it just as if it
// had arrived from the network, so a copy that arrives (or arrived) live is the same message, and later reactions,
// replies and edits find it.
type ImportedMessage struct {
	*bridgev2.ConvertedMessage
	ID        networkid.MessageID
	Sender    bridgev2.EventSender
	Timestamp time.Time
	// Other IDs the message has gone by, eg before it was edited on networks that give edits their own IDs. A copy
	// stored under any of them counts as the same message.
	OtherIDs []networkid.MessageID
	// Earlier versions of an edited message's text, oldest first. The last part of ConvertedMessage is the current
	// text.
	Revisions []ImportedRevision
	Reactions []*bridgev2.BackfillReaction
	// Set if the message was deleted for everyone: a placeholder is kept, without the content.
	DeletedAt time.Time
	// For the owner's messages: "sent" or "delivered", if the export says. Who read them is in ReadBy.
	Status string
	ReadBy []ImportedReceipt
	// For others' messages: whether the owner had read it.
	Read bool
}

type ImportedRevision struct {
	Timestamp time.Time
	Content   *event.MessageEventContent
}

type ImportedReceipt struct {
	Sender networkid.UserID
	At     time.Time
}

// ImportCounts is what ImportMessages did.
type ImportCounts struct {
	Imported int
	// Already here, eg delivered live, or imported before.
	Skipped int
}

// ImportChat makes sure the chat exists, and returns its conversation ID. A chat that isn't here yet is created
// with what the connector knows of it, or, if the connector can't say (eg a group the account has since left), with
// fallback (if not nil). An existing chat is left as it is.
func (s *Service) ImportChat(ctx context.Context, key networkid.PortalKey, fallback *bridgev2.ChatInfo) (int64, error) {
	login := s.CurrentLogin(ctx)
	if login == nil {
		return 0, ErrNotLoggedIn
	}
	portal, err := s.br.GetPortalByKey(ctx, key)
	if err != nil {
		return 0, err
	}
	if portal.MXID == "" {
		if err = portal.CreateMatrixRoom(ctx, login, nil); err != nil && fallback != nil {
			err = portal.CreateMatrixRoom(ctx, login, fallback)
		}
		if err != nil {
			return 0, fmt.Errorf("creating chat %s: %w", key.ID, err)
		}
	}
	return s.store.ConversationByRoom(ctx, string(portal.MXID))
}

// ImportMessages stores messages in a chat created with ImportChat, skipping any that are already here. They go in
// through the chat's own queue, so they don't interleave with messages arriving from the network meanwhile.
func (s *Service) ImportMessages(ctx context.Context, key networkid.PortalKey, msgs []*ImportedMessage) (ImportCounts, error) {
	login := s.CurrentLogin(ctx)
	if login == nil {
		return ImportCounts{}, ErrNotLoggedIn
	}
	var counts ImportCounts
	var importErr error
	ran := false
	res := s.br.QueueRemoteEvent(login, &simplevent.EventMeta{
		Type:      bridgev2.RemoteEventUnknown,
		PortalKey: key,
		PreHandleFunc: func(ctx context.Context, portal *bridgev2.Portal) {
			ran = true
			counts, importErr = s.importBatch(ctx, login, portal, msgs)
		},
	})
	if res.Error != nil {
		return counts, res.Error
	} else if !ran {
		return counts, fmt.Errorf("chat %s doesn't exist; import it with ImportChat first", key.ID)
	}
	return counts, importErr
}

// importOutput is what importBatch sends to the store and the bridge database.
type importOutput struct {
	events    []*event.Event
	extras    []*bridgev2.MatrixSendExtra
	messages  []*database.Message
	reactions []*database.Reaction
	disappear []*database.DisappearingMessage
	// Applied in the store after the events.
	after []func(tx *store.Tx, conversationID int64) error
}

func (o *importOutput) add(evt *event.Event, extra *bridgev2.MatrixSendExtra) {
	o.events = append(o.events, evt)
	o.extras = append(o.extras, extra)
}

func (s *Service) importBatch(ctx context.Context, login *bridgev2.UserLogin, portal *bridgev2.Portal, msgs []*ImportedMessage) (ImportCounts, error) {
	var counts ImportCounts
	out := &importOutput{}
	for _, msg := range msgs {
		exists, err := s.alreadyHere(ctx, portal, msg)
		if err != nil {
			return counts, err
		} else if exists || len(msg.Parts) == 0 {
			counts.Skipped++
			continue
		}
		if err = s.compileImport(ctx, login, portal, msg, out); err != nil {
			return counts, fmt.Errorf("message %s: %w", msg.ID, err)
		}
		counts.Imported++
	}
	if len(out.events) == 0 {
		return counts, nil
	}
	if _, err := s.br.Matrix.BatchSend(ctx, portal.MXID, &mautrix.ReqBeeperBatchSend{Events: out.events}, out.extras); err != nil {
		return counts, err
	}
	err := s.br.DB.DoTxn(ctx, nil, func(ctx context.Context) error {
		for _, m := range out.messages {
			if err := s.br.DB.Message.Insert(ctx, m); err != nil {
				return err
			}
		}
		for _, r := range out.reactions {
			if err := s.br.DB.Reaction.Upsert(ctx, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return counts, err
	}
	for _, dm := range out.disappear {
		if err = s.br.DisappearLoop.Add(ctx, dm); err != nil {
			return counts, err
		}
	}
	return counts, s.applyLocally(ctx, func(tx *store.Tx) error {
		conversationID, _, err := tx.ConversationByRoom(string(portal.MXID))
		if err != nil {
			return err
		}
		for _, fn := range out.after {
			if err = fn(tx, conversationID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) alreadyHere(ctx context.Context, portal *bridgev2.Portal, msg *ImportedMessage) (bool, error) {
	for _, msgID := range append([]networkid.MessageID{msg.ID}, msg.OtherIDs...) {
		existing, err := s.br.DB.Message.GetFirstPartByID(ctx, portal.Receiver, msgID)
		if err != nil || existing != nil {
			return existing != nil, err
		}
	}
	return false, nil
}

// compileImport turns a message into events (its parts, its earlier versions as edits, its reactions, and its
// deletion) and the bridge's records of them, as the bridge's own backfill does.
func (s *Service) compileImport(ctx context.Context, login *bridgev2.UserLogin, portal *bridgev2.Portal, msg *ImportedMessage, out *importOutput) error {
	intent, ok := portal.GetIntentFor(ctx, msg.Sender, login, bridgev2.RemoteEventMessage)
	if !ok {
		return errors.New("no user to send as")
	}
	replyTo, err := s.importReplyTarget(ctx, portal, msg.ReplyTo)
	if err != nil {
		return err
	}
	var firstEvent, lastEvent id.EventID
	var partIDs []networkid.PartID
	for i, part := range msg.Parts {
		evtID := s.br.Matrix.GenerateDeterministicEventID(portal.MXID, portal.PortalKey, msg.ID, part.ID)
		dbMessage := &database.Message{
			ID: msg.ID, PartID: part.ID, MXID: evtID, Room: portal.PortalKey, SenderID: msg.Sender.Sender,
			SenderMXID: intent.GetMXID(), Timestamp: msg.Timestamp, Metadata: part.DBMetadata,
			IsDoublePuppeted: intent.IsDoublePuppet(), EditCount: len(msg.Revisions),
		}
		if msg.ReplyTo != nil {
			dbMessage.ReplyTo = *msg.ReplyTo
		}
		if part.DontBridge {
			dbMessage.SetFakeMXID()
			out.messages = append(out.messages, dbMessage)
			continue
		}
		content := part.Content
		if i == len(msg.Parts)-1 && len(msg.Revisions) > 0 {
			// The message as first sent; its later versions follow as edits.
			content = msg.Revisions[0].Content
		}
		if replyTo != "" {
			content.RelatesTo = (&event.RelatesTo{}).SetReplyTo(replyTo)
		}
		content.BeeperDisappearingTimer = msg.Disappear.ToEventContent()
		ts := msg.Timestamp
		if len(msg.Revisions) > 0 {
			ts = msg.Revisions[0].Timestamp
		}
		out.add(&event.Event{
			Sender: intent.GetMXID(), Type: part.Type, Timestamp: ts.UnixMilli(), ID: evtID, RoomID: portal.MXID,
			Content: event.Content{Parsed: content, Raw: part.Extra},
		}, &bridgev2.MatrixSendExtra{MessageMeta: dbMessage, PartIndex: i})
		out.messages = append(out.messages, dbMessage)
		partIDs = append(partIDs, part.ID)
		if firstEvent == "" {
			firstEvent = evtID
		}
		lastEvent = evtID
		if msg.Disappear.Type != event.DisappearingTypeNone && msg.DeletedAt.IsZero() {
			setting := msg.Disappear
			if setting.Type == event.DisappearingTypeAfterSend && setting.DisappearAt.IsZero() {
				setting.DisappearAt = msg.Timestamp.Add(setting.Timer)
			}
			out.disappear = append(out.disappear, &database.DisappearingMessage{
				RoomID: portal.MXID, EventID: evtID, Timestamp: msg.Timestamp, DisappearingSetting: setting,
			})
		}
	}
	if firstEvent == "" {
		return nil
	}
	s.compileRevisions(portal, intent, msg, lastEvent, out)
	s.compileReactions(ctx, login, portal, msg, partIDs, out)
	if !msg.DeletedAt.IsZero() {
		out.add(&event.Event{
			Sender: intent.GetMXID(), Type: event.EventRedaction, Timestamp: msg.DeletedAt.UnixMilli(),
			ID: localmx.RandomEventID(), RoomID: portal.MXID, Redacts: firstEvent,
			Content: event.Content{Parsed: &event.RedactionEventContent{Redacts: firstEvent}},
		}, nil)
	}
	s.compileReceipts(msg, firstEvent, out)
	return nil
}

func (s *Service) importReplyTarget(ctx context.Context, portal *bridgev2.Portal, target *networkid.MessageOptionalPartID) (id.EventID, error) {
	if target == nil {
		return "", nil
	}
	existing, err := s.br.DB.Message.GetFirstOrSpecificPartByID(ctx, portal.Receiver, *target)
	if err != nil {
		return "", err
	} else if existing != nil {
		return existing.MXID, nil
	}
	// Not here (yet): the event ID it will have, if it's imported later.
	return s.br.Matrix.GenerateDeterministicEventID(portal.MXID, portal.PortalKey, target.MessageID, ptr.Val(target.PartID)), nil
}

// compileRevisions adds an edit of the text for each later version of the message, ending with the current one.
func (s *Service) compileRevisions(portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *ImportedMessage, textEvent id.EventID, out *importOutput) {
	if len(msg.Revisions) == 0 {
		return
	}
	versions := append(msg.Revisions[1:], ImportedRevision{Timestamp: msg.Timestamp, Content: msg.Parts[len(msg.Parts)-1].Content})
	for _, v := range versions {
		content := *v.Content
		content.RelatesTo = nil
		content.SetEdit(textEvent)
		out.add(&event.Event{
			Sender: intent.GetMXID(), Type: event.EventMessage, Timestamp: v.Timestamp.UnixMilli(),
			ID: localmx.RandomEventID(), RoomID: portal.MXID, Content: event.Content{Parsed: &content},
		}, nil)
	}
}

func (s *Service) compileReactions(ctx context.Context, login *bridgev2.UserLogin, portal *bridgev2.Portal, msg *ImportedMessage, partIDs []networkid.PartID, out *importOutput) {
	for _, r := range msg.Reactions {
		intent, ok := portal.GetIntentFor(ctx, r.Sender, login, bridgev2.RemoteEventReaction)
		if !ok {
			continue
		}
		part := partIDs[0]
		if r.TargetPart != nil {
			part = *r.TargetPart
		}
		ts := r.Timestamp
		if ts.IsZero() {
			ts = msg.Timestamp.Add(10 * time.Millisecond)
		}
		target := &database.Message{ID: msg.ID, PartID: part, Room: portal.PortalKey}
		dbReaction := &database.Reaction{
			Room: portal.PortalKey, MessageID: msg.ID, MessagePartID: part, SenderID: r.Sender.Sender,
			EmojiID: r.EmojiID, Emoji: r.Emoji, Timestamp: ts, Metadata: r.DBMetadata,
			MXID: s.br.Matrix.GenerateReactionEventID(portal.MXID, target, r.Sender.Sender, r.EmojiID),
		}
		out.add(&event.Event{
			Sender: intent.GetMXID(), Type: event.EventReaction, Timestamp: ts.UnixMilli(), ID: dbReaction.MXID,
			RoomID: portal.MXID,
			Content: event.Content{Parsed: &event.ReactionEventContent{RelatesTo: event.RelatesTo{
				Type:    event.RelAnnotation,
				EventID: s.br.Matrix.GenerateDeterministicEventID(portal.MXID, portal.PortalKey, msg.ID, part),
				Key:     variationselector.Add(r.Emoji),
			}}, Raw: r.ExtraContent},
		}, &bridgev2.MatrixSendExtra{ReactionMeta: dbReaction})
		out.reactions = append(out.reactions, dbReaction)
	}
}

// compileReceipts records the message's delivery and read state, which aren't events of their own.
func (s *Service) compileReceipts(msg *ImportedMessage, firstEvent id.EventID, out *importOutput) {
	sentAt := msg.Timestamp.UnixMilli()
	if msg.Status != "" {
		status := msg.Status
		out.after = append(out.after, func(tx *store.Tx, _ int64) error {
			return tx.SetImportedStatus(string(firstEvent), status)
		})
	}
	for _, r := range msg.ReadBy {
		r := r
		out.after = append(out.after, func(tx *store.Tx, conversationID int64) error {
			personID, err := tx.Person(string(r.Sender))
			if err != nil {
				return err
			}
			return tx.SetReadReceipt(conversationID, personID, sentAt, r.At.UnixMilli())
		})
	}
	if msg.Read && !msg.Sender.IsFromMe {
		out.after = append(out.after, func(tx *store.Tx, conversationID int64) error {
			return tx.SetLastRead(conversationID, sentAt)
		})
	}
}
