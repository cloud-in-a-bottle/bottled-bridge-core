package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// MatchCandidate is an existing conversation an imported chat could belong to.
type MatchCandidate struct {
	ConversationID int64
	Kind           ConversationKind
	Name           string
	Title          string
	OtherName      string
	MemberNames    []string
	Deleted        bool
	LastMessageAt  int64
}

func (s *Store) MatchCandidates(ctx context.Context) ([]MatchCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.kind, `+conversationNameSQL+`, coalesce(c.title, ''), coalesce(op.name, ''),
			c.deleted_at IS NOT NULL, c.last_message_at,
			coalesce((SELECT group_concat(p.name, char(31)) FROM conversation_members cm JOIN people p ON p.id = cm.person_id
				WHERE cm.conversation_id = c.id AND cm.membership = 'join' AND NOT p.is_self AND p.name IS NOT NULL), '')
		FROM conversations c LEFT JOIN people op ON op.id = c.other_person_id
		WHERE c.kind != 'space'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MatchCandidate
	for rows.Next() {
		var c MatchCandidate
		var members string
		if err = rows.Scan(&c.ConversationID, &c.Kind, &c.Name, &c.Title, &c.OtherName, &c.Deleted, &c.LastMessageAt, &members); err != nil {
			return nil, err
		}
		if members != "" {
			c.MemberNames = strings.Split(members, "\x1f")
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SelfName is the account owner's display name, or "" before anyone has logged in.
func (s *Store) SelfName(ctx context.Context) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, "SELECT coalesce(name, '') FROM people WHERE is_self").Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// PersonByName finds the person an imported chat means by a display name: a member of the conversation if one has
// that name, else anyone with it, else a new person known only by name.
func (tx *Tx) PersonByName(conversationID int64, name string) (int64, error) {
	var personID int64
	err := tx.queryRow(`
		SELECT p.id FROM people p LEFT JOIN conversation_members cm ON cm.person_id = p.id AND cm.conversation_id = ?
		WHERE p.name = ? AND NOT p.is_self ORDER BY cm.person_id IS NULL, p.remote_id LIKE 'name:%' LIMIT 1`,
		conversationID, name,
	).Scan(&personID)
	if err == nil {
		return personID, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	remoteID := NamePrefix + name
	if err = tx.SetPersonName(remoteID, name); err != nil {
		return 0, err
	}
	return tx.Person(remoteID)
}

// HasSimilarMessage reports whether the conversation already has a message (from the bridge, not an earlier import)
// by this author within toleranceMS of sentAt with the same text. A message without text matches any message from
// the author at that time.
func (tx *Tx) HasSimilarMessage(conversationID, authorID, sentAt, toleranceMS int64, body string) (bool, error) {
	var exists bool
	err := tx.queryRow(`
		SELECT EXISTS (SELECT 1 FROM messages WHERE conversation_id = ? AND author_id = ? AND source != 'import'
			AND sent_at BETWEEN ? AND ? AND (? = '' OR coalesce(body, '') = ? OR deleted_at IS NOT NULL))`,
		conversationID, authorID, sentAt-toleranceMS, sentAt+toleranceMS, body, body,
	).Scan(&exists)
	return exists, err
}

// NewImportedConversation creates a conversation for an imported chat that has no counterpart from the bridge.
func (tx *Tx) NewImportedConversation(key string, kind ConversationKind) (int64, error) {
	return tx.UpsertConversation("import:"+key, "", kind)
}

func (tx *Tx) ConversationExists(conversationID int64) error {
	var exists bool
	if err := tx.queryRow("SELECT EXISTS (SELECT 1 FROM conversations WHERE id = ?)", conversationID).Scan(&exists); err != nil {
		return err
	} else if !exists {
		return ErrNotFound
	}
	return nil
}

func (tx *Tx) HasRemoteMessage(conversationID int64, remoteID string) (bool, error) {
	var exists bool
	err := tx.queryRow(
		"SELECT EXISTS (SELECT 1 FROM messages WHERE conversation_id = ? AND remote_id = ?)", conversationID, remoteID,
	).Scan(&exists)
	return exists, err
}

// FileReferenced reports whether anything still refers to a stored file.
func (s *Store) FileReferenced(ctx context.Context, sha string) (bool, error) {
	var used bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM attachments WHERE file_sha256 = ?1)
			OR EXISTS (SELECT 1 FROM people WHERE avatar_sha256 = ?1)
			OR EXISTS (SELECT 1 FROM conversations WHERE avatar_sha256 = ?1)`, sha,
	).Scan(&used)
	return used, err
}

// OtherPerson is a DM's other participant, or 0 for groups and DMs where it isn't known.
func (tx *Tx) OtherPerson(conversationID int64) (int64, error) {
	var personID sql.NullInt64
	err := tx.queryRow("SELECT other_person_id FROM conversations WHERE id = ? AND kind = 'dm'", conversationID).Scan(&personID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return personID.Int64, err
}
