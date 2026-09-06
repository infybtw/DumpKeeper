package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotIncludesWALAndRemainsIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.sql.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`PRAGMA wal_autocheckpoint=0`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
		`INSERT INTO settings (key, value) VALUES ('snapshot-test', 'before')`,
	} {
		if _, err := store.sql.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	wal, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if wal.Size() <= 32 {
		t.Fatal("expected committed data in WAL before snapshot")
	}

	// Download handlers reserve an empty, private destination before snapshotting.
	out, err := os.CreateTemp(t.TempDir(), "snapshot-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := store.Snapshot(context.Background(), out.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE settings SET value = 'after' WHERE key = 'snapshot-test'`); err != nil {
		t.Fatal(err)
	}

	// Open just the downloaded file, without the live database's WAL or SHM.
	copyDB, err := sql.Open("sqlite", "file:"+out.Name()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var value string
	if err := copyDB.QueryRow(`SELECT value FROM settings WHERE key = 'snapshot-test'`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "before" {
		t.Fatalf("snapshot value = %q, want before", value)
	}
	if err := copyDB.QueryRow(`PRAGMA integrity_check`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "ok" {
		t.Fatalf("snapshot integrity: %s", value)
	}
}
