package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

const schemaVersionKey = "schema_version"

// migrations bring a database written by an earlier version up to schema.sql, in order; a database at version n
// has had the first n applied. Append only. schema.sql always describes the latest version, so a new database starts
// there and skips them all.
var migrations = []string{
	// 1: people.fbid was the Messenger user ID, from before the store served other networks.
	"ALTER TABLE people RENAME COLUMN fbid TO remote_id",
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err = migrateTx(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

func migrateTx(tx *sql.Tx) error {
	var tables int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'people'").Scan(&tables); err != nil {
		return err
	}
	isNew := tables == 0
	// For an existing database, the CREATE ... IF NOT EXISTS statements only add what's missing (eg the kv table,
	// on a database from before it existed); they don't change existing tables, which is what the migrations do.
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("failed to apply schema: %w", err)
	}
	version := 0
	var raw string
	err := tx.QueryRow("SELECT value FROM kv WHERE key = ?", schemaVersionKey).Scan(&raw)
	switch {
	case isNew:
		version = len(migrations)
	case errors.Is(err, sql.ErrNoRows):
		// From before versioning: version 0.
	case err != nil:
		return err
	default:
		if version, err = strconv.Atoi(raw); err != nil {
			return fmt.Errorf("invalid schema version %q", raw)
		}
	}
	if version > len(migrations) {
		return fmt.Errorf("the database is at schema version %d, newer than this app knows (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		if _, err = tx.Exec(migrations[i]); err != nil {
			return fmt.Errorf("schema migration %d: %w", i+1, err)
		}
	}
	_, err = tx.Exec(
		"INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
		schemaVersionKey, strconv.Itoa(len(migrations)),
	)
	return err
}
