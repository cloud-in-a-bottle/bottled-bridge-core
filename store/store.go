package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"sync"
	"time"

	// Registers the sqlite3-fk-wal driver (mattn/go-sqlite3 with foreign keys, WAL and a busy timeout).
	_ "go.mau.fi/util/dbutil/litestream"
)

//go:embed schema.sql
var schema string

// Store is the message database. Writes go through Tx, which serializes them; reads can run concurrently.
type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3-fk-wal", fmt.Sprintf("file:%s?_txlock=immediate", path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err = migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Tx is a write transaction.
type Tx struct {
	tx *sql.Tx
	// Conversations whose messages changed, so the caller can tell browsers.
	touched map[int64]struct{}
	// Attachment files that lost their last reference, to delete after commit.
	orphaned []string
}

// TxResult is what a committed transaction changed.
type TxResult struct {
	Touched  []int64
	Orphaned []string
}

func (s *Store) Tx(ctx context.Context, fn func(tx *Tx) error) (TxResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TxResult{}, err
	}
	tx := &Tx{tx: sqlTx, touched: make(map[int64]struct{})}
	if err = fn(tx); err != nil {
		return TxResult{}, errors.Join(err, sqlTx.Rollback())
	}
	if err = sqlTx.Commit(); err != nil {
		return TxResult{}, err
	}
	res := TxResult{Orphaned: tx.orphaned}
	for id := range tx.touched {
		res.Touched = append(res.Touched, id)
	}
	return res, nil
}

func (tx *Tx) exec(query string, args ...any) (sql.Result, error) {
	return tx.tx.Exec(query, args...)
}

func (tx *Tx) queryRow(query string, args ...any) *sql.Row {
	return tx.tx.QueryRow(query, args...)
}

func (tx *Tx) touch(conversationID int64) {
	tx.touched[conversationID] = struct{}{}
}

func (s *Store) GetKV(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM kv WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

func (tx *Tx) SetKV(key, value string) error {
	_, err := tx.exec("INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

func nowMS() int64 {
	return time.Now().UnixMilli()
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullInt(i int64) sql.NullInt64 {
	return sql.NullInt64{Int64: i, Valid: i != 0}
}
