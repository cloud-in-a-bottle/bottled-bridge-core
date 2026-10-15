package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ConversationTarget is what's needed to address events to a conversation.
type ConversationTarget struct {
	RoomID string
	// The newest message the network knows about, for read receipts. Empty if there is none.
	LatestEventID string
	LatestSentAt  int64
}

// ErrReadOnly is returned for conversations that only hold imported history.
var ErrReadOnly = errors.New("this chat only has imported history, and isn't connected to a chat on the network")

func (s *Store) ConversationTarget(ctx context.Context, conversationID int64) (ConversationTarget, error) {
	var t ConversationTarget
	err := s.db.QueryRowContext(ctx, "SELECT room_id FROM conversations WHERE id = ?", conversationID).Scan(&t.RoomID)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	} else if err != nil {
		return t, err
	} else if strings.HasPrefix(t.RoomID, "import:") {
		return t, ErrReadOnly
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT p.event_id, m.sent_at FROM messages m JOIN message_parts p ON p.message_id = m.id
		WHERE m.conversation_id = ? AND m.kind = 'message' AND m.remote_id IS NOT NULL
		ORDER BY m.sent_at DESC, p.rowid DESC LIMIT 1`, conversationID,
	).Scan(&t.LatestEventID, &t.LatestSentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, nil
	}
	return t, err
}

// FirstPart returns the event ID of a message's first part, which is how replies and reactions refer to it.
func (s *Store) FirstPart(ctx context.Context, messageID int64) (string, error) {
	var eventID string
	err := s.db.QueryRowContext(
		ctx, "SELECT event_id FROM message_parts WHERE message_id = ? ORDER BY rowid LIMIT 1", messageID,
	).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return eventID, err
}

type OwnReaction struct {
	EventID string
	Emoji   string
}

// MessageTarget is what's needed to react to, edit or delete a message.
type MessageTarget struct {
	RoomID       string
	FirstEventID string
	// The part holding the message text (the first part if there is no text part).
	TextEventID  string
	OwnReactions []OwnReaction
}

func (s *Store) MessageTarget(ctx context.Context, messageID int64) (MessageTarget, error) {
	var t MessageTarget
	err := s.db.QueryRowContext(ctx, `
		SELECT c.room_id FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE m.id = ?`, messageID,
	).Scan(&t.RoomID)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	} else if err != nil {
		return t, err
	} else if strings.HasPrefix(t.RoomID, "import:") {
		return t, ErrReadOnly
	}
	if t.FirstEventID, err = s.FirstPart(ctx, messageID); err != nil {
		return t, err
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT event_id FROM message_parts WHERE message_id = ? ORDER BY attachment_id IS NOT NULL, rowid LIMIT 1`,
		messageID,
	).Scan(&t.TextEventID)
	if err != nil {
		return t, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.event_id, r.emoji FROM reactions r JOIN people p ON p.id = r.reactor_id
		WHERE r.message_id = ? AND p.is_self`, messageID,
	)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var r OwnReaction
		if err = rows.Scan(&r.EventID, &r.Emoji); err != nil {
			return t, err
		}
		t.OwnReactions = append(t.OwnReactions, r)
	}
	return t, rows.Err()
}

// Unsent is a message from here that the network never accepted, with what's needed to send it again.
type Unsent struct {
	ConversationID int64
	Body           string
	ReplyToEvent   string
	// Hashes of the attachment files, with their captions folded into Body.
	Files []UnsentFile
}

type UnsentFile struct {
	SHA256      string
	Filename    string
	ContentType string
	Width       int
	Height      int
}

// UnsentMessage returns ErrNotFound unless the message is one of ours that failed to send.
func (s *Store) UnsentMessage(ctx context.Context, messageID int64) (Unsent, error) {
	var u Unsent
	err := s.db.QueryRowContext(ctx, `
		SELECT conversation_id, coalesce(body, ''), coalesce(reply_to_event, '') FROM messages
		WHERE id = ? AND status = 'failed' AND remote_id IS NULL`, messageID,
	).Scan(&u.ConversationID, &u.Body, &u.ReplyToEvent)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	} else if err != nil {
		return u, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT file_sha256, coalesce(filename, ''), coalesce(content_type, ''), coalesce(width, 0), coalesce(height, 0),
			coalesce(caption, '')
		FROM attachments WHERE message_id = ? AND status = 'present' ORDER BY ordinal`, messageID)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var f UnsentFile
		var caption string
		if err = rows.Scan(&f.SHA256, &f.Filename, &f.ContentType, &f.Width, &f.Height, &caption); err != nil {
			return u, err
		}
		if caption != "" {
			u.Body = caption
		}
		u.Files = append(u.Files, f)
	}
	return u, rows.Err()
}
