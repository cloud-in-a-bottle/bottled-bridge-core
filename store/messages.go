package store

import (
	"database/sql"
	"errors"
	"fmt"
)

type Source string

const (
	SourceLive     Source = "live"
	SourceBackfill Source = "backfill"
	SourceSent     Source = "sent"
	// From an uploaded "secure storage" export of end-to-end encrypted chats.
	SourceImport Source = "import"
)

// NewAttachment is a media part of a message.
type NewAttachment struct {
	MsgType     string
	ContentType string
	Filename    string
	Size        int64
	Width       int
	Height      int
	DurationMS  int
	Caption     string
	Blurhash    string
	IsVoiceNote bool
	IsSticker   bool
	// Empty if the bridge couldn't fetch the file.
	FileSHA256 string
}

// NewPart is one part of a message as delivered by the bridge. Parts sharing a RemoteID are one message.
type NewPart struct {
	ConversationID  int64
	AuthorID        int64
	RemoteID        string
	EventID         string
	PartID          string
	SentAt          int64
	Kind            string
	MsgType         string
	Body            string
	FormattedBody   string
	ReplyToEvent    string
	ThreadRootEvent string
	LinkPreviews    string
	ExpiresAt       int64
	// The message's disappearing-messages timer, whether or not it has started.
	DisappearTimerMS int64
	ViewOnce         bool
	// An update that doesn't count as chat activity (see the quiet column).
	Quiet  bool
	Status string
	Source Source
	// Set for media parts; Body is then ignored in favour of Attachment.Caption.
	Attachment *NewAttachment
}

// PutPart stores a message part. It is idempotent: delivering the same event (or the same remote part) again
// changes nothing.
func (tx *Tx) PutPart(p NewPart) (int64, error) {
	var messageID int64
	err := tx.queryRow("SELECT message_id FROM message_parts WHERE event_id = ?", p.EventID).Scan(&messageID)
	if err == nil {
		return messageID, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	if p.RemoteID != "" {
		err = tx.queryRow(
			"SELECT id FROM messages WHERE conversation_id = ? AND remote_id = ?", p.ConversationID, p.RemoteID,
		).Scan(&messageID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	if messageID == 0 {
		var body, formatted string
		if p.Attachment == nil {
			body, formatted = p.Body, p.FormattedBody
		}
		err = tx.queryRow(
			`INSERT INTO messages (conversation_id, author_id, remote_id, sent_at, received_at, kind, msgtype, body,
				formatted_body, reply_to_event, thread_root_event, link_previews, expires_at, disappear_timer_ms, view_once,
				quiet, status, source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			p.ConversationID, p.AuthorID, nullString(p.RemoteID), p.SentAt, nowMS(), p.Kind, nullString(p.MsgType),
			nullString(body), nullString(formatted), nullString(p.ReplyToEvent), nullString(p.ThreadRootEvent),
			nullString(p.LinkPreviews), nullInt(p.ExpiresAt), p.DisappearTimerMS, p.ViewOnce, p.Quiet, nullString(p.Status),
			p.Source,
		).Scan(&messageID)
		if err != nil {
			return 0, fmt.Errorf("failed to insert message: %w", err)
		}
	} else {
		// Another part of a message we already have. If it's a part we already stored under a different event ID
		// (eg our own message echoed back), only record the new event ID.
		var existingAttachment sql.NullInt64
		err = tx.queryRow(
			"SELECT attachment_id FROM message_parts WHERE message_id = ? AND part_id = ?", messageID, p.PartID,
		).Scan(&existingAttachment)
		if err == nil {
			_, err = tx.exec(
				"INSERT INTO message_parts (event_id, message_id, part_id, attachment_id) VALUES (?, ?, ?, ?)",
				p.EventID, messageID, p.PartID, existingAttachment,
			)
			return messageID, err
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if p.Attachment == nil && p.Body != "" {
			_, err = tx.exec(
				"UPDATE messages SET body = coalesce(body || char(10), '') || ?, formatted_body = coalesce(formatted_body, ?) WHERE id = ?",
				p.Body, nullString(p.FormattedBody), messageID,
			)
			if err != nil {
				return 0, err
			}
		}
		if p.LinkPreviews != "" {
			if _, err = tx.exec("UPDATE messages SET link_previews = ? WHERE id = ?", p.LinkPreviews, messageID); err != nil {
				return 0, err
			}
		}
	}

	var attachmentID sql.NullInt64
	if a := p.Attachment; a != nil {
		status := "present"
		if a.FileSHA256 == "" {
			status = "missing"
		}
		err = tx.queryRow(
			`INSERT INTO attachments (message_id, ordinal, msgtype, content_type, filename, size, width, height,
				duration_ms, caption, blurhash, is_voice_note, is_sticker, file_sha256, status)
			VALUES (?, (SELECT count(*) FROM attachments WHERE message_id = ?), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id`,
			messageID, messageID, a.MsgType, nullString(a.ContentType), nullString(a.Filename), nullInt(a.Size),
			nullInt(int64(a.Width)), nullInt(int64(a.Height)), nullInt(int64(a.DurationMS)), nullString(a.Caption),
			nullString(a.Blurhash), a.IsVoiceNote, a.IsSticker, nullString(a.FileSHA256), status,
		).Scan(&attachmentID)
		if err != nil {
			return 0, fmt.Errorf("failed to insert attachment: %w", err)
		}
	}
	_, err = tx.exec(
		"INSERT INTO message_parts (event_id, message_id, part_id, attachment_id) VALUES (?, ?, ?, ?)",
		p.EventID, messageID, p.PartID, attachmentID,
	)
	if err != nil {
		return 0, err
	}
	if !p.Quiet {
		if _, err = tx.exec(
			"UPDATE conversations SET last_message_at = max(last_message_at, ?) WHERE id = ?", p.SentAt, p.ConversationID,
		); err != nil {
			return 0, err
		}
	}
	tx.touch(p.ConversationID)
	return messageID, nil
}

// PartRef locates the message (and attachment, for media parts) behind an event ID.
type PartRef struct {
	MessageID      int64
	ConversationID int64
	AttachmentID   int64
	SentAt         int64
	RemoteID       string
}

func (tx *Tx) PartByEvent(eventID string) (PartRef, error) {
	var ref PartRef
	var attachmentID sql.NullInt64
	var remoteID sql.NullString
	err := tx.queryRow(
		`SELECT p.message_id, m.conversation_id, p.attachment_id, m.sent_at, m.remote_id
		FROM message_parts p JOIN messages m ON m.id = p.message_id WHERE p.event_id = ?`, eventID,
	).Scan(&ref.MessageID, &ref.ConversationID, &attachmentID, &ref.SentAt, &remoteID)
	if errors.Is(err, sql.ErrNoRows) {
		return ref, ErrNotFound
	}
	ref.AttachmentID = attachmentID.Int64
	ref.RemoteID = remoteID.String
	return ref, err
}

// EditPart replaces the text of a part: the message body for a text part, or the caption for a media part. The
// replaced text is kept in message_edits.
func (tx *Tx) EditPart(targetEventID, body, formattedBody string, editedAt int64) error {
	ref, err := tx.PartByEvent(targetEventID)
	if err != nil {
		return err
	}
	if ref.AttachmentID != 0 {
		_, err = tx.exec("UPDATE attachments SET caption = ? WHERE id = ?", nullString(body), ref.AttachmentID)
	} else {
		_, err = tx.exec(
			`INSERT INTO message_edits (message_id, revision, body, edited_at)
			SELECT id, (SELECT count(*) FROM message_edits WHERE message_id = ?), body, ? FROM messages WHERE id = ?`,
			ref.MessageID, editedAt, ref.MessageID,
		)
		if err == nil {
			_, err = tx.exec(
				"UPDATE messages SET body = ?, formatted_body = ? WHERE id = ?",
				nullString(body), nullString(formattedBody), ref.MessageID,
			)
		}
	}
	if err != nil {
		return err
	}
	_, err = tx.exec("UPDATE messages SET edited_at = ? WHERE id = ?", editedAt, ref.MessageID)
	tx.touch(ref.ConversationID)
	return err
}

type RedactMode int

const (
	// RedactUnsend leaves a "deleted" placeholder with the content removed.
	RedactUnsend RedactMode = iota
	// RedactDisappear removes the message entirely, as its disappearing timer ran out.
	RedactDisappear
	// RedactViewed removes a view-once message's media, keeping the message.
	RedactViewed
)

// Redact applies a redaction of a reaction or a message part. Unknown targets are ignored: the bridge also redacts
// events this store doesn't keep.
func (tx *Tx) Redact(targetEventID string, at int64, mode RedactMode) error {
	var conversationID int64
	err := tx.queryRow(
		"DELETE FROM reactions WHERE event_id = ? RETURNING (SELECT conversation_id FROM messages WHERE id = message_id)",
		targetEventID,
	).Scan(&conversationID)
	if err == nil {
		tx.touch(conversationID)
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	ref, err := tx.PartByEvent(targetEventID)
	if errors.Is(err, ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if err = tx.purgeAttachments(ref.MessageID); err != nil {
		return err
	}
	switch mode {
	case RedactDisappear:
		_, err = tx.exec("DELETE FROM messages WHERE id = ?", ref.MessageID)
	case RedactViewed:
		_, err = tx.exec("UPDATE messages SET body = NULL, formatted_body = NULL WHERE id = ?", ref.MessageID)
	default:
		if _, err = tx.exec("DELETE FROM message_edits WHERE message_id = ?", ref.MessageID); err != nil {
			return err
		}
		if _, err = tx.exec("DELETE FROM reactions WHERE message_id = ?", ref.MessageID); err != nil {
			return err
		}
		_, err = tx.exec(
			"UPDATE messages SET body = NULL, formatted_body = NULL, link_previews = NULL, deleted_at = ? WHERE id = ?",
			at, ref.MessageID,
		)
	}
	tx.touch(ref.ConversationID)
	return err
}

// purgeAttachments removes a message's media, and queues files nothing else refers to for deletion.
func (tx *Tx) purgeAttachments(messageID int64) error {
	rows, err := tx.tx.Query(
		"SELECT DISTINCT file_sha256 FROM attachments WHERE message_id = ? AND file_sha256 IS NOT NULL", messageID,
	)
	if err != nil {
		return err
	}
	var shas []string
	for rows.Next() {
		var sha string
		if err = rows.Scan(&sha); err != nil {
			_ = rows.Close()
			return err
		}
		shas = append(shas, sha)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if _, err = tx.exec(
		"UPDATE attachments SET status = 'purged', file_sha256 = NULL WHERE message_id = ? AND file_sha256 IS NOT NULL",
		messageID,
	); err != nil {
		return err
	}
	for _, sha := range shas {
		var used bool
		err = tx.queryRow(
			`SELECT EXISTS (SELECT 1 FROM attachments WHERE file_sha256 = ?1)
				OR EXISTS (SELECT 1 FROM people WHERE avatar_sha256 = ?1)
				OR EXISTS (SELECT 1 FROM conversations WHERE avatar_sha256 = ?1)`, sha,
		).Scan(&used)
		if err != nil {
			return err
		}
		if !used {
			tx.orphaned = append(tx.orphaned, sha)
		}
	}
	return nil
}

// PutReaction records a reaction. Some networks allow one reaction per person per message, but the schema allows
// several, keyed by emoji.
func (tx *Tx) PutReaction(eventID, targetEventID string, reactorID int64, emoji string, sentAt int64) error {
	ref, err := tx.PartByEvent(targetEventID)
	if err != nil {
		return err
	}
	_, err = tx.exec(
		`INSERT INTO reactions (event_id, message_id, reactor_id, emoji, sent_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (message_id, reactor_id, emoji) DO UPDATE SET event_id = excluded.event_id, sent_at = excluded.sent_at`,
		eventID, ref.MessageID, reactorID, emoji, sentAt,
	)
	tx.touch(ref.ConversationID)
	return err
}

func (tx *Tx) SetReadReceipt(conversationID, personID, readUpTo, at int64) error {
	_, err := tx.exec(
		`INSERT INTO read_receipts (conversation_id, person_id, read_up_to, at) VALUES (?, ?, ?, ?)
		ON CONFLICT (conversation_id, person_id) DO UPDATE SET read_up_to = max(read_up_to, excluded.read_up_to),
			at = excluded.at`,
		conversationID, personID, readUpTo, at,
	)
	tx.touch(conversationID)
	return err
}

// SetStatus updates the delivery status of a message sent from here. remoteID is filled in once the network assigns one.
func (tx *Tx) SetStatus(eventID, status, errorText, remoteID string) error {
	ref, err := tx.PartByEvent(eventID)
	if err != nil {
		return err
	}
	_, err = tx.exec(
		"UPDATE messages SET status = ?, error = ?, remote_id = coalesce(remote_id, ?) WHERE id = ? AND status IS NOT NULL",
		status, nullString(errorText), nullString(remoteID), ref.MessageID,
	)
	tx.touch(ref.ConversationID)
	return err
}

// SetImportedStatus sets how far a message the owner sent got, for one imported from an export (which, unlike one sent
// from here, has no status yet).
func (tx *Tx) SetImportedStatus(eventID, status string) error {
	ref, err := tx.PartByEvent(eventID)
	if err != nil {
		return err
	}
	_, err = tx.exec("UPDATE messages SET status = ? WHERE id = ?", status, ref.MessageID)
	tx.touch(ref.ConversationID)
	return err
}

// PutUpdate adds a conversation update (eg a rename) to the timeline. msgtype says what changed, body describes it.
func (tx *Tx) PutUpdate(conversationID, authorID int64, eventID string, at int64, msgtype, body string, source Source) error {
	_, err := tx.PutPart(NewPart{
		ConversationID: conversationID,
		AuthorID:       authorID,
		EventID:        eventID,
		SentAt:         at,
		Kind:           "update",
		MsgType:        msgtype,
		Body:           body,
		Source:         source,
	})
	return err
}

func (tx *Tx) PutRawEvent(roomID, eventID, eventType, sender string, ts int64, content string) error {
	_, err := tx.exec(
		"INSERT INTO raw_events (room_id, event_id, type, sender, ts, content) VALUES (?, ?, ?, ?, ?, ?)",
		roomID, eventID, eventType, sender, ts, content,
	)
	return err
}

// DeleteUnsent removes a message from here that never reached the network.
func (tx *Tx) DeleteUnsent(messageID int64) error {
	var conversationID int64
	err := tx.queryRow(
		"SELECT conversation_id FROM messages WHERE id = ? AND status = 'failed' AND remote_id IS NULL", messageID,
	).Scan(&conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if err = tx.purgeAttachments(messageID); err != nil {
		return err
	}
	_, err = tx.exec("DELETE FROM messages WHERE id = ?", messageID)
	tx.touch(conversationID)
	return err
}
