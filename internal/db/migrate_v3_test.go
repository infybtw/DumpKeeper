package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateV3JobEnabled builds the current jobs table by hand except that
// jobs.enabled is still missing, then opens it via Open and asserts the
// migrated shape: existing jobs default to enabled, the flag persists
// through SetJobEnabled and CreateJob, and idempotent re-open keeps it.
func TestMigrateV3JobEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE databases (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 5432,
  username TEXT NOT NULL, password TEXT NOT NULL,
  dbname TEXT NOT NULL, sslmode TEXT NOT NULL DEFAULT 'prefer',
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
)`)
	old.Exec(`CREATE TABLE jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  database_id INTEGER NOT NULL REFERENCES databases(id),
  schedule TEXT NOT NULL DEFAULT '',
  dest_local INTEGER NOT NULL DEFAULT 1,
  keep_last INTEGER NOT NULL DEFAULT 7,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
)`)
	must := func(_ sql.Result, err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(old.Exec(`INSERT INTO databases (name, host, port, username, password, dbname, created_at, updated_at)
		VALUES ('prof','h1',5432,'u','p','d','t1','t1')`))
	must(old.Exec(`INSERT INTO jobs (name, database_id, schedule, created_at, updated_at)
		VALUES ('nightly',1,'0 2 * * *','t1','t1')`))
	old.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open+migrate: %v", err)
	}
	defer store.Close()

	// Pre-existing job migrates as enabled, schedule intact.
	j, err := store.GetJob(1)
	if err != nil {
		t.Fatal(err)
	}
	if !j.Enabled || j.Schedule != "0 2 * * *" || j.Name != "nightly" {
		t.Fatalf("migrated job wrong: %+v", j)
	}

	// Flag flips and survives a re-open (migration must be idempotent).
	if err := store.SetJobEnabled(1, false); err != nil {
		t.Fatalf("set job enabled: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer store.Close()
	if j, err := store.GetJob(1); err != nil || j.Enabled {
		t.Fatalf("disabled flag not persisted: %+v (%v)", j, err)
	}

	// New inserts persist the flag in both states.
	d, err := store.CreateDatabase(Database{Name: "d2", Host: "h2", DBName: "x", Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	off, err := store.CreateJob(Job{Name: "paused", DatabaseID: d.ID, Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	on, err := store.CreateJob(Job{Name: "active", DatabaseID: d.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.ListJobs()
	byName := map[string]bool{}
	for _, j := range got {
		byName[j.Name] = j.Enabled
	}
	if byName["nightly"] || byName["paused"] || !byName["active"] {
		t.Fatalf("enabled flags wrong after roundtrip: %v (ids %d,%d)", byName, off.ID, on.ID)
	}

	// Full UpdateJob keeps carrying the flag.
	j, err = store.GetJob(off.ID)
	if err != nil {
		t.Fatal(err)
	}
	j.Name = "resumed"
	j.Enabled = true
	if err := store.UpdateJob(j); err != nil {
		t.Fatal(err)
	}
	if j, err = store.GetJob(off.ID); err != nil || !j.Enabled || j.Name != "resumed" {
		t.Fatalf("update lost enabled: %+v (%v)", j, err)
	}
}
