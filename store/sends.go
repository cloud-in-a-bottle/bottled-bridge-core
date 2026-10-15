package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// SentFor returns the messages a send with this client ID created, and whether there was one.
func (s *Store) SentFor(ctx context.Context, clientID string) ([]int64, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT message_ids FROM service_sends WHERE client_id = ?", clientID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	var ids []int64
	if err = json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, false, err
	}
	return ids, true, nil
}

// RecordSent remembers the messages a send with this client ID created.
func (tx *Tx) RecordSent(clientID string, messageIDs []int64) error {
	raw, err := json.Marshal(messageIDs)
	if err != nil {
		return err
	}
	_, err = tx.exec("INSERT INTO service_sends (client_id, message_ids, created_at) VALUES (?, ?, ?)",
		clientID, string(raw), nowMS())
	return err
}
