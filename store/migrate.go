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
var migrations = []func(tx *sql.Tx) error{
	// 1: people.fbid was the Messenger user ID, from before the store served other networks.
	execMigration("ALTER TABLE people RENAME COLUMN fbid TO remote_id"),
	// 2: view-once media.
	addColumn("messages", "view_once", "INTEGER NOT NULL DEFAULT 0"),
	// 3: updates that don't count as activity in the chat list.
	addColumn("messages", "quiet", "INTEGER NOT NULL DEFAULT 0"),
	// 4: each message's own disappearing-messages timer.
	addColumn("messages", "disappear_timer_ms", "INTEGER NOT NULL DEFAULT 0"),
}

func execMigration(query string) func(tx *sql.Tx) error {
	return func(tx *sql.Tx) error {
		_, err := tx.Exec(query)
		return err
	}
}

// addColumn adds a column to a table that doesn't have it yet. (A table that was missing altogether has just been
// created by schema.sql, with the column.)
func addColumn(table, column, definition string) func(tx *sql.Tx) error {
	return func(tx *sql.Tx) error {
		var exists bool
		err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM pragma_table_info(?) WHERE name = ?)", table, column).Scan(&exists)
		if err != nil || exists {
			return err
		}
		_, err = tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
		return err
	}
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
		if err = migrations[i](tx); err != nil {
			return fmt.Errorf("schema migration %d: %w", i+1, err)
		}
	}
	_, err = tx.Exec(
		"INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
		schemaVersionKey, strconv.Itoa(len(migrations)),
	)
	return err
}
