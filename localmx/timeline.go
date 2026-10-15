package localmx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"go.mau.fi/util/variationselector"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

// timelineEvent is a non-state event the bridge sends into a room, live or in a backfill batch.
type timelineEvent struct {
	roomID  id.RoomID
	eventID id.EventID
	evtType event.Type
	sender  id.UserID
	ts      int64
	content *event.Content
	// The bridge's record of the remote message this event is (a part of), if any.
	meta   *database.Message
	source store.Source
}

// personFor maps a Matrix user ID the bridge uses to a person row.
func (c *Connector) personFor(ctx context.Context, tx *store.Tx, userID id.UserID) (int64, error) {
	switch userID {
	case OwnerMXID:
		return tx.SelfPerson()
	case BotMXID:
		return tx.Person(store.BotRemoteID)
	}
	ghostID, ok := ParseGhostMXID(userID)
	if !ok {
		return 0, fmt.Errorf("unknown user %s", userID)
	}
	return tx.Person(string(ghostID))
}

func (c *Connector) applyTimelineEvent(ctx context.Context, tx *store.Tx, te timelineEvent) error {
	if te.content.Parsed == nil {
		if err := te.content.ParseRaw(te.evtType); err != nil && !errors.Is(err, event.ErrUnsupportedContentType) {
			return fmt.Errorf("failed to parse content: %w", err)
		}
	}
	raw, err := json.Marshal(te.content)
	if err != nil {
		return err
	}
	if err = tx.PutRawEvent(string(te.roomID), string(te.eventID), te.evtType.Type, string(te.sender), te.ts, string(raw)); err != nil {
		return err
	}
	conversationID, _, err := tx.ConversationByRoom(string(te.roomID))
	if err != nil {
		return fmt.Errorf("room %s: %w", te.roomID, err)
	}
	authorID, err := c.personFor(ctx, tx, te.sender)
	if err != nil {
		return err
	}
	log := zerolog.Ctx(ctx).With().Stringer("event_id", te.eventID).Logger()

	if action, _ := te.content.Raw["com.beeper.action_message"].(map[string]any); action != nil && action["type"] == "disappearing_timer" {
		// The bridge announces a disappearing-timer change with this notice as well as the change itself, which is
		// already in the timeline.
		return nil
	}
	switch te.evtType {
	case event.EventMessage, event.EventSticker:
		msg, ok := te.content.Parsed.(*event.MessageEventContent)
		if !ok {
			return fmt.Errorf("unexpected content %T for %s", te.content.Parsed, te.evtType.Type)
		}
		if target := msg.RelatesTo.GetReplaceID(); target != "" {
			newContent := msg.NewContent
			if newContent == nil {
				newContent = msg
			}
			err = tx.EditPart(string(target), newContent.Body, newContent.FormattedBody, te.ts)
			if errors.Is(err, store.ErrNotFound) {
				log.Warn().Stringer("target", target).Msg("Edit target not found")
				return nil
			}
			return err
		}
		part := c.convertMessage(conversationID, authorID, te, msg)
		_, err = tx.PutPart(part)
		return err
	case event.EventReaction:
		reaction, ok := te.content.Parsed.(*event.ReactionEventContent)
		if !ok {
			return fmt.Errorf("unexpected content %T for reaction", te.content.Parsed)
		}
		emoji := variationselector.Remove(reaction.RelatesTo.Key)
		err = tx.PutReaction(string(te.eventID), string(reaction.RelatesTo.EventID), authorID, emoji, te.ts)
		if errors.Is(err, store.ErrNotFound) {
			log.Debug().Stringer("target", reaction.RelatesTo.EventID).Msg("Reaction target not found")
			return nil
		}
		return err
	case event.EventRedaction:
		redaction, ok := te.content.Parsed.(*event.RedactionEventContent)
		if !ok {
			return fmt.Errorf("unexpected content %T for redaction", te.content.Parsed)
		}
		mode := store.RedactUnsend
		if te.sender == BotMXID && redaction.Reason == "Message disappeared" {
			mode = store.RedactDisappear
			if viewLimited, _ := te.content.Raw["com.beeper.view_limited"].(bool); viewLimited {
				mode = store.RedactViewed
			}
		}
		return tx.Redact(string(redaction.Redacts), te.ts, mode)
	default:
		log.Debug().Str("event_type", te.evtType.Type).Msg("Keeping only the raw copy of unhandled event type")
		return nil
	}
}

func (c *Connector) convertMessage(conversationID, authorID int64, te timelineEvent, msg *event.MessageEventContent) store.NewPart {
	msg.RemoveReplyFallback()
	part := store.NewPart{
		ConversationID:  conversationID,
		AuthorID:        authorID,
		EventID:         string(te.eventID),
		SentAt:          te.ts,
		Kind:            "message",
		MsgType:         string(msg.MsgType),
		Body:            msg.Body,
		ReplyToEvent:    string(msg.RelatesTo.GetNonFallbackReplyTo()),
		ThreadRootEvent: string(msg.RelatesTo.GetThreadParent()),
		Source:          te.source,
	}
	if msg.Format == event.FormatHTML {
		part.FormattedBody = msg.FormattedBody
	}
	if te.meta != nil {
		part.RemoteID = string(te.meta.ID)
		part.PartID = string(te.meta.PartID)
	}
	if te.sender == BotMXID {
		// The bridge's own notices, eg "failed to bridge this message".
		part.Kind = "update"
		part.MsgType = "notice"
	}
	if timer := msg.BeeperDisappearingTimer; timer != nil && timer.Type != event.DisappearingTypeNone && timer.Timer.Duration > 0 {
		part.DisappearTimerMS = timer.Timer.Milliseconds()
		if timer.Type == event.DisappearingTypeAfterSend {
			part.ExpiresAt = te.ts + timer.Timer.Milliseconds()
		}
	}
	if len(msg.BeeperLinkPreviews) > 0 {
		if previews, err := json.Marshal(msg.BeeperLinkPreviews); err == nil {
			part.LinkPreviews = string(previews)
		}
	}
	if msg.MsgType == event.MsgLocation && msg.GeoURI != "" && !strings.Contains(msg.Body, msg.GeoURI) {
		part.Body = strings.TrimSpace(msg.Body + "\n" + msg.GeoURI)
	}
	part.ViewOnce = msg.BeeperViewLimited != nil
	isSticker := te.evtType == event.EventSticker
	if isSticker || msg.MsgType.IsMedia() {
		part.Attachment = convertAttachment(msg, isSticker)
	}
	return part
}

func convertAttachment(msg *event.MessageEventContent, isSticker bool) *store.NewAttachment {
	a := &store.NewAttachment{
		MsgType:     string(msg.MsgType),
		Filename:    msg.FileName,
		IsVoiceNote: msg.MSC3245Voice != nil,
		IsSticker:   isSticker,
		FileSHA256:  SHAFromContentURI(msg.URL),
	}
	if isSticker {
		a.MsgType = "m.sticker"
	}
	if a.Filename == "" {
		a.Filename = msg.Body
	} else if msg.Body != msg.FileName {
		a.Caption = msg.Body
	}
	if info := msg.Info; info != nil {
		a.ContentType = info.MimeType
		a.Size = int64(info.Size)
		a.Width = info.Width
		a.Height = info.Height
		a.DurationMS = info.Duration
		a.Blurhash = info.Blurhash
		if a.Blurhash == "" {
			a.Blurhash = info.AnoaBlurhash
		}
	}
	return a
}

// sendTimelineEvent stores an event the bridge sends live, and returns its event ID.
func (c *Connector) sendTimelineEvent(ctx context.Context, sender id.UserID, roomID id.RoomID, evtType event.Type, content *event.Content, extra *bridgev2.MatrixSendExtra) (id.EventID, error) {
	te := timelineEvent{
		roomID:  roomID,
		evtType: evtType,
		sender:  sender,
		ts:      nowMS(),
		content: content,
		source:  store.SourceLive,
	}
	if extra != nil {
		te.meta = extra.MessageMeta
		if ts := timestampMS(extra.Timestamp); ts != 0 {
			te.ts = ts
		}
	}
	te.eventID = RandomEventID()
	if content.Parsed == nil {
		_ = content.ParseRaw(evtType)
	}
	switch evtType {
	case event.EventMessage, event.EventSticker:
		msg, ok := content.Parsed.(*event.MessageEventContent)
		if ok && msg.RelatesTo.GetReplaceID() == "" && te.meta != nil {
			te.eventID = MessageEventID(te.meta.Room, te.meta.ID, te.meta.PartID)
		}
	case event.EventReaction:
		if extra != nil && extra.ReactionMeta != nil {
			// Same inputs as GenerateReactionEventID, which the bridge uses for backfilled reactions.
			r := extra.ReactionMeta
			te.eventID = ReactionEventID(r.Room, r.MessageID, r.MessagePartID, r.SenderID, string(r.EmojiID))
		}
	}
	err := c.write(ctx, func(tx *store.Tx) error {
		return c.applyTimelineEvent(ctx, tx, te)
	})
	return te.eventID, err
}
