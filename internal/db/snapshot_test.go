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

func TestRestoreSnapshotReplacesConfiguration(t *testing.T) {
	dir := t.TempDir()
	source, err := Open(filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	dbe, err := source.CreateDatabase(Database{Name: "imported", Host: "db.example", Port: 5432, Username: "postgres", Password: "secret", DBName: "app", SSLMode: "require"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.CreateJob(Job{Name: "daily", DatabaseID: dbe.ID, DestLocal: true, Enabled: true, KeepLast: 5}); err != nil {
		t.Fatal(err)
	}
	if err := source.SetSetting(SettingPingInterval, "600"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.CreateTemp(dir, "snapshot-*.db")
	if err != nil {
		t.Fatal(err)
	}
	snapshotPath := snapshot.Name()
	snapshot.Close()
	if err := source.Snapshot(context.Background(), snapshotPath); err != nil {
		t.Fatal(err)
	}
	source.Close()

	target, err := Open(filepath.Join(dir, "target.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := target.CreateDatabase(Database{Name: "old", Host: "localhost", Port: 5432, Username: "postgres", DBName: "postgres", SSLMode: "disable"}); err != nil {
		t.Fatal(err)
	}
	if err := target.RestoreSnapshot(context.Background(), snapshotPath); err != nil {
		t.Fatal(err)
	}
	dbs, err := target.ListDatabases()
	if err != nil {
		t.Fatal(err)
	}
	if len(dbs) != 1 || dbs[0].Name != "imported" {
		t.Fatalf("databases after restore = %+v, want imported database only", dbs)
	}
	jobs, err := target.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Name != "daily" || jobs[0].DatabaseID != dbs[0].ID {
		t.Fatalf("jobs after restore = %+v, want imported daily job", jobs)
	}
	interval, err := target.GetSetting(SettingPingInterval)
	if err != nil || interval != "600" {
		t.Fatalf("ping interval after restore = %q, %v; want 600", interval, err)
	}
}

func TestRestoreSnapshotRejectsEmptyDatabase(t *testing.T) {
	dir := t.TempDir()
	target, err := Open(filepath.Join(dir, "target.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	empty := filepath.Join(dir, "empty.db")
	if _, err := os.Create(empty); err != nil {
		t.Fatal(err)
	}
	if err := target.RestoreSnapshot(context.Background(), empty); err == nil {
		t.Fatal("RestoreSnapshot unexpectedly accepted an empty database")
	}
}
