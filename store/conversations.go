package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrNotFound is returned when a looked-up row doesn't exist.
var ErrNotFound = errors.New("not found")

type ConversationKind string

const (
	KindDM    ConversationKind = "dm"
	KindGroup ConversationKind = "group"
	KindSpace ConversationKind = "space"
)

// UpsertConversation returns the conversation for a room, creating it (or reviving a deleted one).
func (tx *Tx) UpsertConversation(roomID, threadKey string, kind ConversationKind) (int64, error) {
	var id int64
	err := tx.queryRow(
		"INSERT INTO conversations (room_id, thread_key, kind, created_at) VALUES (?, ?, ?, ?) "+
			"ON CONFLICT (room_id) DO UPDATE SET kind = excluded.kind, deleted_at = NULL RETURNING id",
		roomID, threadKey, kind, nowMS(),
	).Scan(&id)
	if err == nil {
		tx.touch(id)
	}
	return id, err
}

// ConversationByRoom returns ErrNotFound for rooms the bridge never created.
func (tx *Tx) ConversationByRoom(roomID string) (int64, ConversationKind, error) {
	var id int64
	var kind ConversationKind
	err := tx.queryRow("SELECT id, kind FROM conversations WHERE room_id = ?", roomID).Scan(&id, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return id, kind, err
}

func (tx *Tx) MarkConversationDeleted(roomID string) error {
	_, err := tx.exec("UPDATE conversations SET deleted_at = ? WHERE room_id = ? AND deleted_at IS NULL", nowMS(), roomID)
	return err
}

// The setters below update one derived column each; callers pass column names from this package only.
func (tx *Tx) setConversation(id int64, column string, value any) error {
	_, err := tx.exec("UPDATE conversations SET "+column+" = ? WHERE id = ?", value, id)
	if err == nil {
		tx.touch(id)
	}
	return err
}

func (tx *Tx) SetConversationTitle(id int64, title string) error {
	return tx.setConversation(id, "title", nullString(title))
}

func (tx *Tx) SetConversationTopic(id int64, topic string) error {
	return tx.setConversation(id, "topic", nullString(topic))
}

func (tx *Tx) SetConversationAvatar(id int64, sha string) error {
	return tx.setConversation(id, "avatar_sha256", nullString(sha))
}

func (tx *Tx) SetConversationParent(id int64, parentID int64) error {
	return tx.setConversation(id, "parent_id", nullInt(parentID))
}

func (tx *Tx) SetConversationOtherPerson(id int64, personID int64) error {
	return tx.setConversation(id, "other_person_id", nullInt(personID))
}

func (tx *Tx) SetConversationPinned(id int64, pinned bool) error {
	return tx.setConversation(id, "pinned", pinned)
}

func (tx *Tx) SetConversationArchived(id int64, archived bool) error {
	return tx.setConversation(id, "archived", archived)
}

func (tx *Tx) SetConversationMutedUntil(id int64, until int64) error {
	return tx.setConversation(id, "muted_until", until)
}

func (tx *Tx) SetConversationMarkedUnread(id int64, unread bool) error {
	return tx.setConversation(id, "marked_unread", unread)
}

func (tx *Tx) SetConversationDisappearing(id int64, disappearType string, timerMS int64) error {
	if err := tx.setConversation(id, "disappear_type", nullString(disappearType)); err != nil {
		return err
	}
	return tx.setConversation(id, "disappear_timer_ms", timerMS)
}

// SetLastRead moves the conversation's read marker forward (never back), and clears a manual unread mark.
func (tx *Tx) SetLastRead(id int64, readUpTo int64) error {
	_, err := tx.exec(
		"UPDATE conversations SET last_read_at = max(last_read_at, ?), marked_unread = 0 WHERE id = ?", readUpTo, id,
	)
	if err == nil {
		tx.touch(id)
	}
	return err
}

func (tx *Tx) SetMembership(conversationID, personID int64, membership string) (previous string, err error) {
	err = tx.queryRow(
		"SELECT membership FROM conversation_members WHERE conversation_id = ? AND person_id = ?", conversationID, personID,
	).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	_, err = tx.exec(
		"INSERT INTO conversation_members (conversation_id, person_id, membership) VALUES (?, ?, ?) "+
			"ON CONFLICT (conversation_id, person_id) DO UPDATE SET membership = excluded.membership",
		conversationID, personID, membership,
	)
	if err == nil {
		tx.touch(conversationID)
	}
	return previous, err
}

func (tx *Tx) SetRoomState(roomID, eventType, stateKey, sender, content string) error {
	_, err := tx.exec(
		"INSERT INTO room_state (room_id, type, state_key, sender, content, updated_at) VALUES (?, ?, ?, ?, ?, ?) "+
			"ON CONFLICT (room_id, type, state_key) DO UPDATE SET sender = excluded.sender, content = excluded.content, "+
			"updated_at = excluded.updated_at",
		roomID, eventType, stateKey, sender, content, nowMS(),
	)
	return err
}

// StateEntry is one piece of room state.
type StateEntry struct {
	StateKey string
	Sender   string
	Content  string
}

// RoomState returns the room's state of one type, keyed by state key.
func (s *Store) RoomState(ctx context.Context, roomID, eventType string) (map[string]StateEntry, error) {
	rows, err := s.db.QueryContext(
		ctx, "SELECT state_key, sender, content FROM room_state WHERE room_id = ? AND type = ?", roomID, eventType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]StateEntry)
	for rows.Next() {
		var e StateEntry
		if err = rows.Scan(&e.StateKey, &e.Sender, &e.Content); err != nil {
			return nil, err
		}
		out[e.StateKey] = e
	}
	return out, rows.Err()
}
