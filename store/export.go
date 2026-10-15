package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The Export* reads are for apps that write the history back out, eg as a backup in the network's own format: every
// person, conversation and message, with the network's IDs.

type ExportPerson struct {
	RemoteID string
	Name     string
	IsSelf   bool
}

func (s *Store) ExportPeople(ctx context.Context) ([]ExportPerson, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT remote_id, coalesce(name, ''), is_self FROM people ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportPerson
	for rows.Next() {
		var p ExportPerson
		if err = rows.Scan(&p.RemoteID, &p.Name, &p.IsSelf); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type ExportMember struct {
	RemoteID   string
	Membership string
}

type ExportReceipt struct {
	RemoteID string
	// How far they've read, as a message timestamp, and when.
	ReadUpTo int64
	At       int64
}

type ExportConversation struct {
	ID        int64
	RoomID    string
	ThreadKey string
	Kind      ConversationKind
	Title     string
	Pinned    bool
	Archived  bool
	// Unix milliseconds, or -1 for muted until unmuted.
	MutedUntil       int64
	DisappearType    string
	DisappearTimerMS int64
	LastReadAt       int64
	Deleted          bool
	// Only has imported history, with no chat on the network behind it.
	ReadOnly     bool
	Members      []ExportMember
	ReadReceipts []ExportReceipt
}

func (s *Store) ExportConversations(ctx context.Context) ([]ExportConversation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, room_id, thread_key, kind, coalesce(title, ''), pinned, archived, muted_until,
			coalesce(disappear_type, ''), disappear_timer_ms, last_read_at, deleted_at IS NOT NULL, room_id LIKE 'import:%'
		FROM conversations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []ExportConversation
	byID := make(map[int64]int)
	for rows.Next() {
		var c ExportConversation
		if err = rows.Scan(&c.ID, &c.RoomID, &c.ThreadKey, &c.Kind, &c.Title, &c.Pinned, &c.Archived, &c.MutedUntil,
			&c.DisappearType, &c.DisappearTimerMS, &c.LastReadAt, &c.Deleted, &c.ReadOnly); err != nil {
			_ = rows.Close()
			return nil, err
		}
		byID[c.ID] = len(out)
		out = append(out, c)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	err = s.eachRow(ctx, `SELECT cm.conversation_id, p.remote_id, cm.membership FROM conversation_members cm
		JOIN people p ON p.id = cm.person_id ORDER BY cm.conversation_id, p.id`, func(rows *sql.Rows) error {
		var conversationID int64
		var m ExportMember
		if err := rows.Scan(&conversationID, &m.RemoteID, &m.Membership); err != nil {
			return err
		}
		c := &out[byID[conversationID]]
		c.Members = append(c.Members, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = s.eachRow(ctx, `SELECT rr.conversation_id, p.remote_id, rr.read_up_to, rr.at FROM read_receipts rr
		JOIN people p ON p.id = rr.person_id ORDER BY rr.conversation_id, p.id`, func(rows *sql.Rows) error {
		var conversationID int64
		var r ExportReceipt
		if err := rows.Scan(&conversationID, &r.RemoteID, &r.ReadUpTo, &r.At); err != nil {
			return err
		}
		c := &out[byID[conversationID]]
		c.ReadReceipts = append(c.ReadReceipts, r)
		return nil
	})
	return out, err
}

type ExportEdit struct {
	// The text before this edit replaced it, and when it was replaced.
	Body     string
	EditedAt int64
}

type ExportAttachment struct {
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
	// Empty unless Status is "present".
	FileSHA256 string
	Status     string
}

type ExportReaction struct {
	ReactorRemoteID string
	Emoji           string
	SentAt          int64
}

// ExportReplyTarget is the message a reply quotes.
type ExportReplyTarget struct {
	RemoteID       string
	AuthorRemoteID string
	SentAt         int64
	Body           string
}

type ExportMessage struct {
	ID             int64
	ConversationID int64
	AuthorRemoteID string
	// The network's message ID; empty for messages without one (eg from an export without IDs).
	RemoteID         string
	SentAt           int64
	ReceivedAt       int64
	Kind             string
	MsgType          string
	Body             string
	FormattedBody    string
	ReplyTo          *ExportReplyTarget
	LinkPreviews     string
	EditedAt         int64
	DeletedAt        int64
	ExpiresAt        int64
	DisappearTimerMS int64
	ViewOnce         bool
	Quiet            bool
	Status           string
	Source           Source
	// Earlier versions, oldest first.
	Edits       []ExportEdit
	Attachments []ExportAttachment
	Reactions   []ExportReaction
}

// exportPage is how many messages ExportMessages loads at a time.
const exportPage = 500

// ExportMessages calls fn with every message, in timeline order (by when they were sent), without holding them all in
// memory.
func (s *Store) ExportMessages(ctx context.Context, fn func(ExportMessage) error) error {
	after := Position{SentAt: -1 << 62}
	for {
		page, err := s.exportPage(ctx, after)
		if err != nil {
			return err
		}
		for _, m := range page {
			if err = fn(m); err != nil {
				return err
			}
		}
		if len(page) < exportPage {
			return nil
		}
		last := page[len(page)-1]
		after = Position{SentAt: last.SentAt, ID: last.ID}
	}
}

func (s *Store) exportPage(ctx context.Context, after Position) ([]ExportMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.conversation_id, a.remote_id, coalesce(m.remote_id, ''), m.sent_at, m.received_at, m.kind,
			coalesce(m.msgtype, ''), coalesce(m.body, ''), coalesce(m.formatted_body, ''), coalesce(m.link_previews, ''),
			coalesce(m.edited_at, 0), coalesce(m.deleted_at, 0), coalesce(m.expires_at, 0), m.disappear_timer_ms,
			m.view_once, m.quiet,
			coalesce(m.status, ''), m.source,
			coalesce(q.remote_id, ''), coalesce(qa.remote_id, ''), coalesce(q.sent_at, 0), coalesce(q.body, '')
		FROM messages m
		JOIN people a ON a.id = m.author_id
		LEFT JOIN message_parts qp ON qp.event_id = m.reply_to_event
		LEFT JOIN messages q ON q.id = qp.message_id
		LEFT JOIN people qa ON qa.id = q.author_id
		WHERE (m.sent_at, m.id) > (?, ?) ORDER BY m.sent_at, m.id LIMIT ?`, after.SentAt, after.ID, exportPage)
	if err != nil {
		return nil, err
	}
	var page []ExportMessage
	for rows.Next() {
		var m ExportMessage
		var q ExportReplyTarget
		if err = rows.Scan(&m.ID, &m.ConversationID, &m.AuthorRemoteID, &m.RemoteID, &m.SentAt, &m.ReceivedAt, &m.Kind,
			&m.MsgType, &m.Body, &m.FormattedBody, &m.LinkPreviews, &m.EditedAt, &m.DeletedAt, &m.ExpiresAt,
			&m.DisappearTimerMS, &m.ViewOnce, &m.Quiet, &m.Status, &m.Source, &q.RemoteID, &q.AuthorRemoteID, &q.SentAt, &q.Body); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if q.SentAt != 0 {
			m.ReplyTo = &q
		}
		page = append(page, m)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil || len(page) == 0 {
		return page, err
	}
	index := make(map[int64]int, len(page))
	ids := make([]string, len(page))
	for i, m := range page {
		index[m.ID] = i
		ids[i] = fmt.Sprint(m.ID)
	}
	inList := strings.Join(ids, ",")
	err = s.eachRow(ctx, `SELECT message_id, coalesce(body, ''), edited_at FROM message_edits
		WHERE message_id IN (`+inList+`) ORDER BY message_id, revision`, func(rows *sql.Rows) error {
		var messageID int64
		var e ExportEdit
		if err := rows.Scan(&messageID, &e.Body, &e.EditedAt); err != nil {
			return err
		}
		m := &page[index[messageID]]
		m.Edits = append(m.Edits, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = s.eachRow(ctx, `SELECT message_id, msgtype, coalesce(content_type, ''), coalesce(filename, ''),
			coalesce(size, 0), coalesce(width, 0), coalesce(height, 0), coalesce(duration_ms, 0), coalesce(caption, ''),
			coalesce(blurhash, ''), is_voice_note, is_sticker, coalesce(file_sha256, ''), status
		FROM attachments WHERE message_id IN (`+inList+`) ORDER BY message_id, ordinal`, func(rows *sql.Rows) error {
		var messageID int64
		var a ExportAttachment
		if err := rows.Scan(&messageID, &a.MsgType, &a.ContentType, &a.Filename, &a.Size, &a.Width, &a.Height,
			&a.DurationMS, &a.Caption, &a.Blurhash, &a.IsVoiceNote, &a.IsSticker, &a.FileSHA256, &a.Status); err != nil {
			return err
		}
		m := &page[index[messageID]]
		m.Attachments = append(m.Attachments, a)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = s.eachRow(ctx, `SELECT r.message_id, p.remote_id, r.emoji, r.sent_at FROM reactions r
		JOIN people p ON p.id = r.reactor_id WHERE r.message_id IN (`+inList+`) ORDER BY r.message_id, r.sent_at`,
		func(rows *sql.Rows) error {
			var messageID int64
			var r ExportReaction
			if err := rows.Scan(&messageID, &r.ReactorRemoteID, &r.Emoji, &r.SentAt); err != nil {
				return err
			}
			m := &page[index[messageID]]
			m.Reactions = append(m.Reactions, r)
			return nil
		})
	return page, err
}

// eachRow runs query and calls fn for each row.
func (s *Store) eachRow(ctx context.Context, query string, fn func(rows *sql.Rows) error, args ...any) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err = fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
