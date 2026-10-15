package bridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/random"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/localmx"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

// OutgoingFile is an attachment already put in the file store.
type OutgoingFile struct {
	SHA256      string
	Filename    string
	ContentType string
	Size        int64
	Width       int
	Height      int
}

// newEvent builds an event from the owner, as if typed in a Matrix client. Its ID carries a transaction ID: one the
// connector understands, if it generates them, so the network's echo of the message is matched to it.
func (s *Service) newEvent(roomID id.RoomID, evtType event.Type, content any) *event.Event {
	txnID := networkid.RawTransactionID(random.String(32))
	if gen, ok := s.opts.Connector.(bridgev2.TransactionIDGeneratingNetwork); ok {
		txnID = gen.GenerateTransactionID(localmx.OwnerMXID, roomID, evtType)
	}
	return &event.Event{
		ID:        id.EventID(database.NetworkTxnMXIDPrefix + string(txnID)),
		Type:      evtType,
		Sender:    localmx.OwnerMXID,
		RoomID:    roomID,
		Timestamp: time.Now().UnixMilli(),
		Content:   event.Content{Parsed: content},
	}
}

// queue hands an event to the bridge and waits for it to be handled. Messages report their final delivery status
// separately (see localmx.Connector.SendMessageStatus).
func (s *Service) queue(evt *event.Event) error {
	ctx := s.log.With().Stringer("event_id", evt.ID).Logger().WithContext(context.Background())
	res := s.br.QueueMatrixEvent(ctx, evt)
	if res.Error != nil {
		return res.Error
	} else if !res.Success && !res.Queued {
		return errors.New("the bridge ignored the event")
	}
	return nil
}

func mediaMsgType(contentType string) event.MessageType {
	switch {
	case len(contentType) >= 6 && contentType[:6] == "image/":
		return event.MsgImage
	case len(contentType) >= 6 && contentType[:6] == "video/":
		return event.MsgVideo
	case len(contentType) >= 6 && contentType[:6] == "audio/":
		return event.MsgAudio
	default:
		return event.MsgFile
	}
}

// Send sends a text and/or files, and returns the last message's ID. See SendAll.
func (s *Service) Send(ctx context.Context, conversationID int64, body string, replyToMessageID int64, files []OutgoingFile) (int64, error) {
	ids, err := s.SendAll(ctx, conversationID, body, replyToMessageID, files)
	if err != nil {
		return 0, err
	}
	return ids[len(ids)-1], nil
}

// SendAll sends a text and/or files. Each file is its own message on the network; the text goes along as the last
// file's caption. It returns the messages' IDs once they're stored locally; sending continues in the background.
func (s *Service) SendAll(ctx context.Context, conversationID int64, body string, replyToMessageID int64, files []OutgoingFile) ([]int64, error) {
	var replyTo id.EventID
	if replyToMessageID != 0 {
		ref, err := s.store.FirstPart(ctx, replyToMessageID)
		if err != nil {
			return nil, fmt.Errorf("reply target: %w", err)
		}
		replyTo = id.EventID(ref)
	}
	return s.send(ctx, conversationID, body, replyTo, files)
}

func (s *Service) send(ctx context.Context, conversationID int64, body string, replyTo id.EventID, files []OutgoingFile) ([]int64, error) {
	target, err := s.store.ConversationTarget(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	var contents []*event.MessageEventContent
	for i, f := range files {
		content := &event.MessageEventContent{
			MsgType:  mediaMsgType(f.ContentType),
			Body:     f.Filename,
			FileName: f.Filename,
			URL:      localmx.ContentURI(f.SHA256),
			Info: &event.FileInfo{
				MimeType: f.ContentType,
				Size:     int(f.Size),
				Width:    f.Width,
				Height:   f.Height,
			},
		}
		if i == len(files)-1 && body != "" {
			content.Body = body
		}
		contents = append(contents, content)
	}
	if len(files) == 0 {
		if body == "" {
			return nil, errors.New("nothing to send")
		}
		contents = append(contents, &event.MessageEventContent{MsgType: event.MsgText, Body: body})
	}

	var evts []*event.Event
	var messageIDs []int64
	_, err = s.store.Tx(ctx, func(tx *store.Tx) error {
		selfID, err := tx.SelfPerson()
		if err != nil {
			return err
		}
		for _, content := range contents {
			if replyTo != "" {
				content.RelatesTo = (&event.RelatesTo{}).SetReplyTo(replyTo)
			}
			evt := s.newEvent(id.RoomID(target.RoomID), event.EventMessage, content)
			part := store.NewPart{
				ConversationID: conversationID,
				AuthorID:       selfID,
				EventID:        string(evt.ID),
				SentAt:         evt.Timestamp,
				Kind:           "message",
				MsgType:        string(content.MsgType),
				Body:           content.Body,
				ReplyToEvent:   string(replyTo),
				Status:         "pending",
				Source:         store.SourceSent,
			}
			if content.URL != "" {
				part.Attachment = &store.NewAttachment{
					MsgType:     string(content.MsgType),
					ContentType: content.Info.MimeType,
					Filename:    content.FileName,
					Size:        int64(content.Info.Size),
					Width:       content.Info.Width,
					Height:      content.Info.Height,
					FileSHA256:  localmx.SHAFromContentURI(content.URL),
				}
				if content.Body != content.FileName {
					part.Attachment.Caption = content.Body
				}
			}
			messageID, err := tx.PutPart(part)
			if err != nil {
				return err
			}
			messageIDs = append(messageIDs, messageID)
			evts = append(evts, evt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.broker.Publish("conversation", events.ConversationChanged{ConversationID: conversationID})
	go func() {
		for _, evt := range evts {
			if err := s.queue(evt); err != nil {
				s.log.Err(err).Stringer("event_id", evt.ID).Msg("Failed to send message")
				s.mx.SendMessageStatus(context.Background(), &bridgev2.MessageStatus{
					Status: event.MessageStatusFail, Message: err.Error(),
				}, &bridgev2.MessageStatusEventInfo{RoomID: evt.RoomID, SourceEventID: evt.ID})
			}
		}
	}()
	return messageIDs, nil
}

// React sets the owner's reaction to a message, replacing any previous one (the networks allow one per person), or
// removes it if emoji is empty.
func (s *Service) React(ctx context.Context, messageID int64, emoji string) error {
	target, err := s.store.MessageTarget(ctx, messageID)
	if err != nil {
		return err
	}
	for _, existing := range target.OwnReactions {
		if existing.Emoji == emoji {
			return nil
		}
		redaction := s.newEvent(id.RoomID(target.RoomID), event.EventRedaction, &event.RedactionEventContent{Redacts: id.EventID(existing.EventID)})
		redaction.Redacts = id.EventID(existing.EventID)
		if err = s.queue(redaction); err != nil {
			return err
		}
		if err = s.applyLocally(ctx, func(tx *store.Tx) error {
			return tx.Redact(existing.EventID, redaction.Timestamp, store.RedactUnsend)
		}); err != nil {
			return err
		}
	}
	if emoji == "" {
		return nil
	}
	evt := s.newEvent(id.RoomID(target.RoomID), event.EventReaction, &event.ReactionEventContent{
		RelatesTo: event.RelatesTo{Type: event.RelAnnotation, EventID: id.EventID(target.FirstEventID), Key: emoji},
	})
	if err = s.queue(evt); err != nil {
		return err
	}
	return s.applyLocally(ctx, func(tx *store.Tx) error {
		selfID, err := tx.SelfPerson()
		if err != nil {
			return err
		}
		return tx.PutReaction(string(evt.ID), target.FirstEventID, selfID, emoji, evt.Timestamp)
	})
}

// Unreact removes the owner's reaction with this emoji, if there is one.
func (s *Service) Unreact(ctx context.Context, messageID int64, emoji string) error {
	target, err := s.store.MessageTarget(ctx, messageID)
	if err != nil {
		return err
	}
	for _, existing := range target.OwnReactions {
		if existing.Emoji != emoji {
			continue
		}
		redaction := s.newEvent(id.RoomID(target.RoomID), event.EventRedaction, &event.RedactionEventContent{Redacts: id.EventID(existing.EventID)})
		redaction.Redacts = id.EventID(existing.EventID)
		if err = s.queue(redaction); err != nil {
			return err
		}
		return s.applyLocally(ctx, func(tx *store.Tx) error {
			return tx.Redact(existing.EventID, redaction.Timestamp, store.RedactUnsend)
		})
	}
	return nil
}

// Retry sends a failed message again, replacing it.
func (s *Service) Retry(ctx context.Context, messageID int64) (int64, error) {
	unsent, err := s.store.UnsentMessage(ctx, messageID)
	if err != nil {
		return 0, err
	}
	files := make([]OutgoingFile, len(unsent.Files))
	for i, f := range unsent.Files {
		files[i] = OutgoingFile{
			SHA256: f.SHA256, Filename: f.Filename, ContentType: f.ContentType, Size: s.files.Size(f.SHA256),
			Width: f.Width, Height: f.Height,
		}
	}
	// Send first, so the files the failed copy shares with the new one are still referenced when it's deleted.
	newIDs, err := s.send(ctx, unsent.ConversationID, unsent.Body, id.EventID(unsent.ReplyToEvent), files)
	if err != nil {
		return 0, err
	}
	return newIDs[len(newIDs)-1], s.Discard(ctx, messageID)
}

// Discard deletes a message that failed to send.
func (s *Service) Discard(ctx context.Context, messageID int64) error {
	return s.applyLocally(ctx, func(tx *store.Tx) error {
		return tx.DeleteUnsent(messageID)
	})
}

// Unsend deletes one of the owner's messages for everyone.
func (s *Service) Unsend(ctx context.Context, messageID int64) error {
	target, err := s.store.MessageTarget(ctx, messageID)
	if err != nil {
		return err
	}
	evt := s.newEvent(id.RoomID(target.RoomID), event.EventRedaction, &event.RedactionEventContent{Redacts: id.EventID(target.FirstEventID)})
	evt.Redacts = id.EventID(target.FirstEventID)
	if err = s.queue(evt); err != nil {
		return err
	}
	return s.applyLocally(ctx, func(tx *store.Tx) error {
		return tx.Redact(target.FirstEventID, evt.Timestamp, store.RedactUnsend)
	})
}

// Edit replaces the text of one of the owner's messages.
func (s *Service) Edit(ctx context.Context, messageID int64, body string) error {
	target, err := s.store.MessageTarget(ctx, messageID)
	if err != nil {
		return err
	}
	// SetEdit makes this the new content and wraps it in the edit (with the "* " fallback body).
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: body}
	content.SetEdit(id.EventID(target.TextEventID))
	evt := s.newEvent(id.RoomID(target.RoomID), event.EventMessage, content)
	if err = s.queue(evt); err != nil {
		return err
	}
	return s.applyLocally(ctx, func(tx *store.Tx) error {
		return tx.EditPart(target.TextEventID, body, "", evt.Timestamp)
	})
}

// MarkRead tells the network the owner has read the conversation up to its latest message.
func (s *Service) MarkRead(ctx context.Context, conversationID int64) error {
	return s.MarkReadUpTo(ctx, conversationID, 0)
}

// MarkReadUpTo tells the network the owner has read the conversation up to a message, or its latest if messageID is
// zero.
func (s *Service) MarkReadUpTo(ctx context.Context, conversationID, messageID int64) error {
	target, err := s.store.ReadTarget(ctx, conversationID, messageID)
	if errors.Is(err, store.ErrReadOnly) {
		// Nothing to tell the network; just stop counting the imported messages as unread.
		return s.applyLocally(ctx, func(tx *store.Tx) error {
			return tx.SetLastRead(conversationID, time.Now().UnixMilli())
		})
	} else if err != nil {
		return err
	}
	if target.LatestEventID == "" {
		return nil
	}
	if err = s.applyLocally(ctx, func(tx *store.Tx) error {
		return tx.SetLastRead(conversationID, target.LatestSentAt)
	}); err != nil {
		return err
	}
	evt := &event.Event{
		Type:   event.EphemeralEventReceipt,
		RoomID: id.RoomID(target.RoomID),
		Content: event.Content{Parsed: &event.ReceiptEventContent{
			id.EventID(target.LatestEventID): {
				event.ReceiptTypeRead: {localmx.OwnerMXID: {Timestamp: time.Now()}},
			},
		}},
	}
	evt.Mautrix.EventSource = event.SourceEphemeral
	go func() {
		if err := s.queue(evt); err != nil {
			s.log.Warn().Err(err).Msg("Failed to send read receipt")
		}
	}()
	return nil
}

func (s *Service) SetTyping(ctx context.Context, conversationID int64, typing bool) error {
	target, err := s.store.ConversationTarget(ctx, conversationID)
	if err != nil {
		return err
	}
	userIDs := []id.UserID{}
	if typing {
		userIDs = append(userIDs, localmx.OwnerMXID)
	}
	evt := &event.Event{
		Type:    event.EphemeralEventTyping,
		RoomID:  id.RoomID(target.RoomID),
		Content: event.Content{Parsed: &event.TypingEventContent{UserIDs: userIDs}},
	}
	evt.Mautrix.EventSource = event.SourceEphemeral
	return s.queue(evt)
}

func (s *Service) applyLocally(ctx context.Context, fn func(tx *store.Tx) error) error {
	res, err := s.store.Tx(ctx, fn)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to apply change to message store")
		return err
	}
	if err = s.files.Delete(res.Orphaned); err != nil {
		s.log.Err(err).Msg("Failed to delete orphaned attachment files")
	}
	for _, conversationID := range res.Touched {
		s.broker.Publish("conversation", events.ConversationChanged{ConversationID: conversationID})
	}
	return nil
}

// ErrNotViewOnce is returned for MarkViewed on a message that isn't view-once media, or was opened already.
var ErrNotViewOnce = errors.New("not an unopened view-once message")

// MarkViewed tells the network the owner opened a view-once message. Its media is then removed here, as on the
// network's own apps.
func (s *Service) MarkViewed(ctx context.Context, messageID int64) error {
	login := s.CurrentLogin(ctx)
	if login == nil {
		return ErrNotLoggedIn
	}
	msg, err := s.store.MessageByID(ctx, messageID)
	if err != nil {
		return err
	} else if !msg.ViewOnce || !slices.ContainsFunc(msg.Attachments, func(a store.Attachment) bool { return a.Status == "present" }) {
		return ErrNotViewOnce
	}
	target, err := s.store.MessageTarget(ctx, messageID)
	if err != nil {
		return err
	}
	return s.br.ViewLimitedMedia(ctx, login, id.EventID(target.FirstEventID), &event.BeeperViewLimitedMedia{Type: "count", Count: 1})
}
