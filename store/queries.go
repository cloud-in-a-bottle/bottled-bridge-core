package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// conversationNameSQL names a conversation (aliased c, with its DM counterpart joined as op): its title, else the
// other person in a DM, else a few members.
const conversationNameSQL = `coalesce(c.title, op.name, (
	SELECT group_concat(name, ', ') FROM (
		SELECT p.name FROM conversation_members cm JOIN people p ON p.id = cm.person_id
		WHERE cm.conversation_id = c.id AND cm.membership = 'join' AND NOT p.is_self AND p.name IS NOT NULL
		ORDER BY p.name LIMIT 4)
), 'Unnamed chat')`

type ConversationSummary struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	AvatarSHA256  string `json:"avatar_sha256,omitempty"`
	OtherPersonID int64  `json:"other_person_id,omitempty"`
	LastMessageAt int64  `json:"last_message_at"`
	Preview       string `json:"last_message_preview"`
	UnreadCount   int64  `json:"unread_count"`
	MarkedUnread  bool   `json:"marked_unread"`
	Pinned        bool   `json:"pinned"`
	Archived      bool   `json:"archived"`
	MutedUntil    int64  `json:"muted_until"`
	DisappearType string `json:"disappear_type,omitempty"`
	DisappearMS   int64  `json:"disappear_timer_ms"`
	Topic         string `json:"topic,omitempty"`
	// Only has history imported from an export, with no chat on the network behind it to send to.
	ReadOnly bool `json:"read_only"`
	// The other person in a DM.
	OtherRemoteID string `json:"-"`
	OtherName     string `json:"-"`
}

const summaryColumns = `c.id, c.kind, ` + conversationNameSQL + `, coalesce(c.avatar_sha256, op.avatar_sha256, ''),
	coalesce(op.remote_id, ''), coalesce(op.name, ''),
	coalesce(c.other_person_id, 0), c.last_message_at, c.marked_unread, c.pinned, c.archived, c.muted_until,
	coalesce(c.disappear_type, ''), c.disappear_timer_ms, coalesce(c.topic, ''), c.room_id LIKE 'import:%',
	(SELECT count(*) FROM messages m JOIN people a ON a.id = m.author_id
		WHERE m.conversation_id = c.id AND m.kind = 'message' AND m.sent_at > c.last_read_at AND NOT a.is_self
		AND m.deleted_at IS NULL AND m.source != 'import'),
	coalesce((SELECT CASE
			WHEN m.kind = 'update' THEN coalesce(a.name, '') || ' ' || coalesce(m.body, '')
			WHEN m.deleted_at IS NOT NULL THEN 'Message deleted'
			ELSE (CASE WHEN a.is_self THEN 'You: ' WHEN c.kind = 'group' THEN coalesce(a.name, '?') || ': ' ELSE '' END)
				|| coalesce(nullif(m.body, ''), (SELECT CASE at.msgtype
					WHEN 'm.image' THEN '📷 Photo' WHEN 'm.video' THEN '🎞 Video' WHEN 'm.audio' THEN '🎤 Audio'
					WHEN 'm.sticker' THEN 'Sticker' ELSE '📄 ' || coalesce(at.filename, 'File') END
					FROM attachments at WHERE at.message_id = m.id ORDER BY at.ordinal LIMIT 1), '')
			END
		FROM messages m JOIN people a ON a.id = m.author_id
		WHERE m.conversation_id = c.id AND NOT m.quiet ORDER BY m.sent_at DESC, m.id DESC LIMIT 1), '')`

const summaryFrom = ` FROM conversations c LEFT JOIN people op ON op.id = c.other_person_id`

func scanSummary(row interface{ Scan(...any) error }) (ConversationSummary, error) {
	var s ConversationSummary
	err := row.Scan(&s.ID, &s.Kind, &s.Name, &s.AvatarSHA256, &s.OtherRemoteID, &s.OtherName, &s.OtherPersonID, &s.LastMessageAt, &s.MarkedUnread,
		&s.Pinned, &s.Archived, &s.MutedUntil, &s.DisappearType, &s.DisappearMS, &s.Topic, &s.ReadOnly, &s.UnreadCount, &s.Preview)
	return s, err
}

func (s *Store) ListConversations(ctx context.Context) ([]ConversationSummary, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+summaryColumns+summaryFrom+
		" WHERE c.deleted_at IS NULL AND c.kind != 'space' ORDER BY c.pinned DESC, c.last_message_at DESC, c.id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConversationSummary{}
	for rows.Next() {
		summary, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, rows.Err()
}

type Member struct {
	PersonID     int64  `json:"person_id"`
	RemoteID     string `json:"remote_id"`
	Name         string `json:"name"`
	AvatarSHA256 string `json:"avatar_sha256,omitempty"`
	IsSelf       bool   `json:"is_self"`
	Membership   string `json:"membership"`
}

type ConversationDetail struct {
	Summary ConversationSummary `json:"summary"`
	Members []Member            `json:"members"`
}

func (s *Store) ConversationDetail(ctx context.Context, conversationID int64) (ConversationDetail, error) {
	var d ConversationDetail
	var err error
	d.Summary, err = scanSummary(s.db.QueryRowContext(ctx, "SELECT "+summaryColumns+summaryFrom+" WHERE c.id = ?", conversationID))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	} else if err != nil {
		return d, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.remote_id, coalesce(p.name, ''), coalesce(p.avatar_sha256, ''), p.is_self, cm.membership
		FROM conversation_members cm JOIN people p ON p.id = cm.person_id
		WHERE cm.conversation_id = ? AND cm.membership IN ('join', 'invite') AND p.remote_id != ?
		ORDER BY p.is_self, p.name`, conversationID, BotRemoteID)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	d.Members = []Member{}
	for rows.Next() {
		var m Member
		if err = rows.Scan(&m.PersonID, &m.RemoteID, &m.Name, &m.AvatarSHA256, &m.IsSelf, &m.Membership); err != nil {
			return d, err
		}
		d.Members = append(d.Members, m)
	}
	return d, rows.Err()
}

type Quote struct {
	MessageID  int64  `json:"message_id"`
	AuthorName string `json:"author_name"`
	Text       string `json:"text"`
}

type Attachment struct {
	ID          int64  `json:"id"`
	MsgType     string `json:"msgtype"`
	ContentType string `json:"content_type"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	Width       int64  `json:"width"`
	Height      int64  `json:"height"`
	DurationMS  int64  `json:"duration_ms"`
	Caption     string `json:"caption"`
	IsVoiceNote bool   `json:"is_voice_note"`
	IsSticker   bool   `json:"is_sticker"`
	Status      string `json:"status"`
}

type Reaction struct {
	Emoji         string `json:"emoji"`
	ReactorID     int64  `json:"reactor_id"`
	ReactorName   string `json:"reactor_name"`
	Mine          bool   `json:"mine"`
	ReactorRemote string `json:"-"`
	ReactorAvatar string `json:"-"`
}

type Message struct {
	ID             int64  `json:"id"`
	ConversationID int64  `json:"-"`
	AuthorID       int64  `json:"author_id"`
	AuthorRemote   string `json:"-"`
	AuthorAvatar   string `json:"-"`
	AuthorName     string `json:"author_name"`
	Outgoing       bool   `json:"outgoing"`
	SentAt         int64  `json:"sent_at"`
	Kind           string `json:"kind"`
	MsgType        string `json:"msgtype"`
	Body           string `json:"body"`
	// HTML with the message's formatting (styles, mentions), if it has any; Body is the plain text.
	FormattedBody string `json:"formatted_body,omitempty"`
	// View-once media, whose attachments are purged once viewed.
	ViewOnce     bool            `json:"view_once,omitempty"`
	EditedAt     int64           `json:"edited_at,omitempty"`
	Deleted      bool            `json:"deleted"`
	ExpiresAt    int64           `json:"expires_at,omitempty"`
	Status       string          `json:"status,omitempty"`
	Error        string          `json:"error,omitempty"`
	Quote        *Quote          `json:"quote"`
	LinkPreviews json.RawMessage `json:"link_previews,omitempty"`
	Attachments  []Attachment    `json:"attachments"`
	Reactions    []Reaction      `json:"reactions"`
}

type MessagePage struct {
	Messages []Message `json:"messages"`
	HasMore  bool      `json:"has_more"`
}

// MessagesPage returns up to limit messages before `before` (or the newest, if nil), oldest first.
func (s *Store) MessagesPage(ctx context.Context, conversationID int64, before *Position, limit int) (MessagePage, error) {
	msgs, more, err := s.Before(ctx, conversationID, before, limit)
	if err != nil {
		return MessagePage{}, err
	}
	return MessagePage{Messages: msgs, HasMore: more}, nil
}

const messageColumns = `m.id, m.conversation_id, m.author_id, a.remote_id, coalesce(a.name, ''),
	coalesce(a.avatar_sha256, ''), a.is_self, m.sent_at, m.kind, coalesce(m.msgtype, ''), coalesce(m.body, ''),
	coalesce(m.formatted_body, ''), m.view_once, coalesce(m.edited_at, 0), m.deleted_at IS NOT NULL, coalesce(m.expires_at, 0), coalesce(m.status, ''),
	coalesce(m.error, ''), coalesce(m.link_previews, ''), coalesce(q.id, 0), coalesce(qa.name, ''), coalesce(q.body, ''),
	(SELECT max(rr.read_up_to) FROM read_receipts rr WHERE rr.conversation_id = m.conversation_id
		AND rr.person_id != m.author_id)`

const messageFrom = ` FROM messages m
	JOIN people a ON a.id = m.author_id
	LEFT JOIN message_parts qp ON qp.event_id = m.reply_to_event
	LEFT JOIN messages q ON q.id = qp.message_id
	LEFT JOIN people qa ON qa.id = q.author_id`

// loadMessages returns the messages matching where (over messages m), in order, with their attachments and
// reactions.
func (s *Store) loadMessages(ctx context.Context, where string, args []any, order string, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+messageColumns+messageFrom+" WHERE "+where+" ORDER BY "+order+
		" LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	msgs := []Message{}
	for rows.Next() {
		var m Message
		var linkPreviews string
		var quoteID int64
		var quoteAuthor, quoteText string
		var readUpTo sql.NullInt64
		if err = rows.Scan(&m.ID, &m.ConversationID, &m.AuthorID, &m.AuthorRemote, &m.AuthorName, &m.AuthorAvatar,
			&m.Outgoing, &m.SentAt, &m.Kind, &m.MsgType, &m.Body, &m.FormattedBody, &m.ViewOnce, &m.EditedAt, &m.Deleted, &m.ExpiresAt, &m.Status,
			&m.Error, &linkPreviews, &quoteID, &quoteAuthor, &quoteText, &readUpTo); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if linkPreviews != "" {
			m.LinkPreviews = json.RawMessage(linkPreviews)
		}
		if quoteID != 0 {
			m.Quote = &Quote{MessageID: quoteID, AuthorName: quoteAuthor, Text: quoteText}
		}
		if m.Outgoing && m.Status != "" && m.Status != "failed" && m.Status != "pending" && readUpTo.Int64 >= m.SentAt {
			m.Status = "read"
		}
		m.Attachments = []Attachment{}
		m.Reactions = []Reaction{}
		msgs = append(msgs, m)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return msgs, nil
	}
	byID := make(map[int64]*Message, len(msgs))
	ids := make([]string, len(msgs))
	for i := range msgs {
		byID[msgs[i].ID] = &msgs[i]
		ids[i] = fmt.Sprint(msgs[i].ID)
	}
	inList := strings.Join(ids, ",")
	if err = s.fillAttachments(ctx, inList, byID); err != nil {
		return nil, err
	}
	return msgs, s.fillReactions(ctx, inList, byID)
}

func (s *Store) fillAttachments(ctx context.Context, inList string, byID map[int64]*Message) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT message_id, id, msgtype, coalesce(content_type, ''), coalesce(filename, ''), coalesce(size, 0),
			coalesce(width, 0), coalesce(height, 0), coalesce(duration_ms, 0), coalesce(caption, ''), is_voice_note,
			is_sticker, status
		FROM attachments WHERE message_id IN (`+inList+`) ORDER BY message_id, ordinal`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID int64
		var a Attachment
		if err = rows.Scan(&messageID, &a.ID, &a.MsgType, &a.ContentType, &a.Filename, &a.Size, &a.Width, &a.Height,
			&a.DurationMS, &a.Caption, &a.IsVoiceNote, &a.IsSticker, &a.Status); err != nil {
			return err
		}
		byID[messageID].Attachments = append(byID[messageID].Attachments, a)
	}
	return rows.Err()
}

func (s *Store) fillReactions(ctx context.Context, inList string, byID map[int64]*Message) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.message_id, r.emoji, r.reactor_id, coalesce(p.name, ''), p.is_self, p.remote_id,
			coalesce(p.avatar_sha256, '')
		FROM reactions r JOIN people p ON p.id = r.reactor_id
		WHERE r.message_id IN (`+inList+`) ORDER BY r.sent_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID int64
		var r Reaction
		if err = rows.Scan(&messageID, &r.Emoji, &r.ReactorID, &r.ReactorName, &r.Mine, &r.ReactorRemote, &r.ReactorAvatar); err != nil {
			return err
		}
		byID[messageID].Reactions = append(byID[messageID].Reactions, r)
	}
	return rows.Err()
}

type MessageLocation struct {
	ConversationID int64 `json:"conversation_id"`
	SentAt         int64 `json:"sent_at"`
}

func (s *Store) MessageLocation(ctx context.Context, messageID int64) (MessageLocation, error) {
	var loc MessageLocation
	err := s.db.QueryRowContext(ctx, "SELECT conversation_id, sent_at FROM messages WHERE id = ?", messageID).
		Scan(&loc.ConversationID, &loc.SentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return loc, ErrNotFound
	}
	return loc, err
}

type AttachmentFile struct {
	SHA256      string
	ContentType string
	Filename    string
}

func (s *Store) AttachmentFile(ctx context.Context, attachmentID int64) (AttachmentFile, error) {
	var f AttachmentFile
	err := s.db.QueryRowContext(ctx, `
		SELECT file_sha256, coalesce(content_type, ''), coalesce(filename, '') FROM attachments
		WHERE id = ? AND status = 'present'`, attachmentID,
	).Scan(&f.SHA256, &f.ContentType, &f.Filename)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

type SearchHit struct {
	ConversationID   int64  `json:"conversation_id"`
	ConversationName string `json:"conversation_name"`
	MessageID        int64  `json:"message_id"`
	SentAt           int64  `json:"sent_at"`
	AuthorName       string `json:"author_name"`
	Snippet          string `json:"snippet"`
}

// Search does a full-text search of message bodies, in one conversation or (if conversationID is zero) all of them.
// Each word of the query matches as a prefix.
func (s *Store) Search(ctx context.Context, query string, conversationID int64, limit int) ([]SearchHit, error) {
	var terms []string
	for word := range strings.FieldsSeq(query) {
		terms = append(terms, `"`+strings.ReplaceAll(word, `"`, `""`)+`"*`)
	}
	hits := []SearchHit{}
	if len(terms) == 0 {
		return hits, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, `+conversationNameSQL+`, m.id, m.sent_at, coalesce(a.name, ''),
			snippet(messages_fts, 0, '[', ']', '…', 12)
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid
		JOIN people a ON a.id = m.author_id
		JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN people op ON op.id = c.other_person_id
		WHERE messages_fts MATCH ? AND m.kind = 'message' AND (? = 0 OR m.conversation_id = ?)
		ORDER BY m.sent_at DESC LIMIT ?`, strings.Join(terms, " "), conversationID, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h SearchHit
		if err = rows.Scan(&h.ConversationID, &h.ConversationName, &h.MessageID, &h.SentAt, &h.AuthorName, &h.Snippet); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

type Contact struct {
	PersonID       int64  `json:"person_id"`
	RemoteID       string `json:"remote_id"`
	Name           string `json:"name"`
	AvatarSHA256   string `json:"avatar_sha256,omitempty"`
	ConversationID int64  `json:"conversation_id,omitempty"`
}

// Contacts are the people in any conversation, with their DM conversation if there is one.
func (s *Store) Contacts(ctx context.Context) ([]Contact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.remote_id, p.name, coalesce(p.avatar_sha256, ''),
			coalesce((SELECT c.id FROM conversations c WHERE c.other_person_id = p.id AND c.kind = 'dm'
				AND c.deleted_at IS NULL ORDER BY c.last_message_at DESC LIMIT 1), 0)
		FROM people p WHERE NOT p.is_self AND p.remote_id != ? AND p.name IS NOT NULL ORDER BY p.name`, BotRemoteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Contact{}
	for rows.Next() {
		var c Contact
		if err = rows.Scan(&c.PersonID, &c.RemoteID, &c.Name, &c.AvatarSHA256, &c.ConversationID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) PersonRemoteID(ctx context.Context, personID int64) (string, error) {
	var remoteID string
	err := s.db.QueryRowContext(ctx, "SELECT remote_id FROM people WHERE id = ?", personID).Scan(&remoteID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return remoteID, err
}

func (s *Store) ConversationByRoom(ctx context.Context, roomID string) (int64, error) {
	var conversationID int64
	err := s.db.QueryRowContext(ctx, "SELECT id FROM conversations WHERE room_id = ?", roomID).Scan(&conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return conversationID, err
}
