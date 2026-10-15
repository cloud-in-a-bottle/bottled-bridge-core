package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// Position is a message's place in a timeline. Messages are ordered by when they were sent, then by row ID, so
// positions are unique even when several messages share a timestamp.
type Position struct {
	SentAt int64
	ID     int64
}

// Before returns up to limit messages before pos (or the newest, if nil), oldest first, and whether there are more.
func (s *Store) Before(ctx context.Context, conversationID int64, pos *Position, limit int) ([]Message, bool, error) {
	where, args := "m.conversation_id = ?", []any{conversationID}
	if pos != nil {
		where += " AND (m.sent_at, m.id) < (?, ?)"
		args = append(args, pos.SentAt, pos.ID)
	}
	msgs, err := s.loadMessages(ctx, where, args, "m.sent_at DESC, m.id DESC", limit+1)
	if err != nil {
		return nil, false, err
	}
	more := len(msgs) > limit
	if more {
		msgs = msgs[:limit]
	}
	slices.Reverse(msgs)
	return msgs, more, nil
}

// After returns up to limit messages after pos, oldest first, and whether there are more.
func (s *Store) After(ctx context.Context, conversationID int64, pos Position, limit int) ([]Message, bool, error) {
	msgs, err := s.loadMessages(ctx, "m.conversation_id = ? AND (m.sent_at, m.id) > (?, ?)",
		[]any{conversationID, pos.SentAt, pos.ID}, "m.sent_at, m.id", limit+1)
	if err != nil {
		return nil, false, err
	}
	more := len(msgs) > limit
	if more {
		msgs = msgs[:limit]
	}
	return msgs, more, nil
}

// MessageByID returns one message, with its attachments and reactions.
func (s *Store) MessageByID(ctx context.Context, messageID int64) (Message, error) {
	msgs, err := s.loadMessages(ctx, "m.id = ?", []any{messageID}, "m.id", 1)
	if err != nil {
		return Message{}, err
	} else if len(msgs) == 0 {
		return Message{}, ErrNotFound
	}
	return msgs[0], nil
}

// MessagesByID returns the messages with these IDs that exist, in timeline order.
func (s *Store) MessagesByID(ctx context.Context, ids []int64) ([]Message, error) {
	if len(ids) == 0 {
		return []Message{}, nil
	}
	args := make([]any, len(ids))
	placeholders := ""
	for i, id := range ids {
		args[i] = id
		if i > 0 {
			placeholders += ", "
		}
		placeholders += "?"
	}
	return s.loadMessages(ctx, "m.id IN ("+placeholders+")", args, "m.sent_at, m.id", len(ids))
}

// RecentQuery selects messages across every conversation, newest first.
type RecentQuery struct {
	// Unix milliseconds; zero for no bound. Since is inclusive, Until exclusive.
	Since, Until int64
	// Continue after this position (exclusive), from a previous page.
	Before *Position
	Limit  int
}

// Recent returns messages across every conversation, newest first, and whether there are more.
func (s *Store) Recent(ctx context.Context, q RecentQuery) ([]Message, bool, error) {
	where, args := "m.kind = 'message'", []any{}
	if q.Since != 0 {
		where += " AND m.sent_at >= ?"
		args = append(args, q.Since)
	}
	if q.Until != 0 {
		where += " AND m.sent_at < ?"
		args = append(args, q.Until)
	}
	if q.Before != nil {
		where += " AND (m.sent_at, m.id) < (?, ?)"
		args = append(args, q.Before.SentAt, q.Before.ID)
	}
	msgs, err := s.loadMessages(ctx, where, args, "m.sent_at DESC, m.id DESC", q.Limit+1)
	if err != nil {
		return nil, false, err
	}
	more := len(msgs) > q.Limit
	if more {
		msgs = msgs[:q.Limit]
	}
	return msgs, more, nil
}

// ChatPosition is a conversation's place in the chat list, newest activity first.
type ChatPosition struct {
	LastMessageAt int64
	ID            int64
}

type Archived string

const (
	ArchivedExclude Archived = "exclude"
	ArchivedInclude Archived = "include"
	ArchivedOnly    Archived = "only"
)

// ConversationsPage lists conversations by latest activity (ignoring pins), after pos if given, and whether there
// are more.
func (s *Store) ConversationsPage(ctx context.Context, archived Archived, after *ChatPosition, limit int) ([]ConversationSummary, bool, error) {
	where, args := " WHERE c.deleted_at IS NULL AND c.kind != 'space'", []any{}
	switch archived {
	case ArchivedExclude:
		where += " AND NOT c.archived"
	case ArchivedOnly:
		where += " AND c.archived"
	case ArchivedInclude:
	default:
		return nil, false, errors.New("unknown archived filter " + string(archived))
	}
	if after != nil {
		where += " AND (c.last_message_at, c.id) < (?, ?)"
		args = append(args, after.LastMessageAt, after.ID)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+summaryColumns+summaryFrom+where+
		" ORDER BY c.last_message_at DESC, c.id DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []ConversationSummary{}
	for rows.Next() {
		summary, err := scanSummary(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, summary)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// ConversationSummary returns one conversation's summary.
func (s *Store) ConversationSummary(ctx context.Context, conversationID int64) (ConversationSummary, error) {
	summary, err := scanSummary(s.db.QueryRowContext(ctx, "SELECT "+summaryColumns+summaryFrom+" WHERE c.id = ?", conversationID))
	if errors.Is(err, sql.ErrNoRows) {
		return summary, ErrNotFound
	}
	return summary, err
}

// ConversationRoom returns a conversation's room ID, which for imported history starts with "import:".
func (s *Store) ConversationRoom(ctx context.Context, conversationID int64) (string, error) {
	var roomID string
	err := s.db.QueryRowContext(ctx, "SELECT room_id FROM conversations WHERE id = ?", conversationID).Scan(&roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return roomID, err
}

// LatestRoomState returns the content of one piece of state from the most recently active conversation of a kind
// that has it, or "" if none does.
func (s *Store) LatestRoomState(ctx context.Context, kind ConversationKind, eventType string) (string, error) {
	var content string
	err := s.db.QueryRowContext(ctx, `
		SELECT rs.content FROM conversations c JOIN room_state rs ON rs.room_id = c.room_id
		WHERE c.kind = ? AND rs.type = ? AND rs.state_key = '' AND c.deleted_at IS NULL
		ORDER BY c.last_message_at DESC LIMIT 1`, kind, eventType).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return content, err
}

// Person is someone in the store.
type Person struct {
	ID           int64
	RemoteID     string
	Name         string
	AvatarSHA256 string
	IsSelf       bool
}

const personColumns = "p.id, p.remote_id, coalesce(p.name, ''), coalesce(p.avatar_sha256, ''), p.is_self"

func scanPerson(row interface{ Scan(...any) error }) (Person, error) {
	var p Person
	err := row.Scan(&p.ID, &p.RemoteID, &p.Name, &p.AvatarSHA256, &p.IsSelf)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// Self is the logged-in account's person; ErrNotFound before a login.
func (s *Store) Self(ctx context.Context) (Person, error) {
	return scanPerson(s.db.QueryRowContext(ctx, "SELECT "+personColumns+" FROM people p WHERE p.is_self"))
}

func (s *Store) PersonByID(ctx context.Context, personID int64) (Person, error) {
	return scanPerson(s.db.QueryRowContext(ctx, "SELECT "+personColumns+" FROM people p WHERE p.id = ?", personID))
}

func (s *Store) PersonByRemoteID(ctx context.Context, remoteID string) (Person, error) {
	return scanPerson(s.db.QueryRowContext(ctx, "SELECT "+personColumns+" FROM people p WHERE p.remote_id = ?", remoteID))
}

// PeopleNamed lists the people on the network the store knows, whose name contains query (all of them, if it's
// empty), by name.
func (s *Store) PeopleNamed(ctx context.Context, query string, limit int) ([]Person, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+personColumns+` FROM people p
		WHERE NOT p.is_self AND p.remote_id != ? AND p.remote_id NOT LIKE ? AND p.name IS NOT NULL
			AND p.name LIKE '%' || ? || '%'
		ORDER BY p.name LIMIT ?`, BotRemoteID, NamePrefix+"%", query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Person{}
	for rows.Next() {
		p, err := scanPerson(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
