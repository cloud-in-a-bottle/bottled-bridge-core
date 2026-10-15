package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "go.mau.fi/util/dbutil/litestream"
)

// The people table as the Messenger-only versions created it, before schema versions.
const unversionedPeople = `
CREATE TABLE people (
    id INTEGER PRIMARY KEY,
    fbid TEXT NOT NULL UNIQUE,
    is_self INTEGER NOT NULL DEFAULT 0,
    name TEXT,
    avatar_sha256 TEXT,
    updated_at INTEGER NOT NULL
);
INSERT INTO people (fbid, is_self, name, updated_at) VALUES ('100', 1, 'Me', 1), ('200', 0, 'Alice', 1);
`

func TestMigratesUnversionedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.db")
	db, err := sql.Open("sqlite3-fk-wal", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(unversionedPeople); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	for range 2 {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var remoteID string
		if err = st.db.QueryRow("SELECT remote_id FROM people WHERE is_self").Scan(&remoteID); err != nil || remoteID != "100" {
			t.Fatalf("self after migration: %q, %v", remoteID, err)
		}
		version, _, err := st.GetKV(t.Context(), schemaVersionKey)
		if err != nil || version != "1" {
			t.Fatalf("schema version %q, %v", version, err)
		}
		_ = st.Close()
	}
}

func TestNewDatabaseStartsAtLatestVersion(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	version, _, err := st.GetKV(t.Context(), schemaVersionKey)
	if err != nil || version != "1" {
		t.Fatalf("schema version %q, %v", version, err)
	}
}
