package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateV1ToV2 builds a pre-2.0 database by hand, then opens it via
// Open and asserts the migrated shape: one database entity per legacy job,
// one destination from the legacy global S3 settings, jobs and backups
// linked to them, and legacy columns gone.
func TestMigrateV1ToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	v1, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	v1.Exec(`CREATE TABLE jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 5432,
  username TEXT NOT NULL, password TEXT NOT NULL,
  dbname TEXT NOT NULL, sslmode TEXT NOT NULL DEFAULT 'prefer',
  schedule TEXT NOT NULL DEFAULT '',
  dest_local INTEGER NOT NULL DEFAULT 1,
  dest_s3 INTEGER NOT NULL DEFAULT 0,
  keep_last INTEGER NOT NULL DEFAULT 7,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
)`)
	v1.Exec(`CREATE TABLE backups (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  status TEXT NOT NULL,
  trigger TEXT NOT NULL,
  started_at TEXT NOT NULL, finished_at TEXT,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  filename TEXT NOT NULL,
  stored_local INTEGER NOT NULL DEFAULT 0,
  stored_s3 INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  restored_at TEXT
)`)
	v1.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	must := func(_ sql.Result, err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(v1.Exec(`INSERT INTO settings VALUES ('s3_endpoint','s3.example.com'),
		('s3_region','eu-west-1'),('s3_bucket','bk'),('s3_prefix','pk'),
		('s3_access_key','ak'),('s3_secret_key','sk'),('s3_use_ssl','1')`))
	must(v1.Exec(`INSERT INTO jobs (name, host, port, username, password, dbname, sslmode, schedule, dest_local, dest_s3, keep_last, created_at, updated_at)
		VALUES ('oldjob','h1',5433,'u1','p1','d1','require','0 2 * * *',1,1,5,'t1','t1'),
		       ('localonly','h2',5434,'u2','p2','d2','prefer','',1,0,3,'t2','t2')`))
	must(v1.Exec(`INSERT INTO backups (job_id, status, trigger, started_at, filename, stored_local, stored_s3)
		VALUES (1,'completed','manual','s1','oldjob-s1.dump',1,1),
		       (2,'completed','cron','s2','localonly-s2.dump',1,0)`))
	v1.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open+migrate: %v", err)
	}
	defer store.Close()

	// Databases: one per legacy job, credentials carried over.
	dbs, err := store.ListDatabases()
	if err != nil || len(dbs) != 2 {
		t.Fatalf("want 2 databases, got %d (%v)", len(dbs), err)
	}
	var dbOldjob Database
	for _, d := range dbs {
		if d.Name == "oldjob" {
			dbOldjob = d
		}
	}
	if dbOldjob.Host != "h1" || dbOldjob.Port != 5433 || dbOldjob.Username != "u1" ||
		dbOldjob.Password != "p1" || dbOldjob.DBName != "d1" || dbOldjob.SSLMode != "require" {
		t.Fatalf("oldjob database not carried over: %+v", dbOldjob)
	}

	// Destination: one, from the legacy settings.
	dests, err := store.ListDestinations()
	if err != nil || len(dests) != 1 {
		t.Fatalf("want 1 destination, got %d (%v)", len(dests), err)
	}
	dest := dests[0]
	if dest.Name != "s3" || dest.Endpoint != "s3.example.com" || dest.Bucket != "bk" ||
		dest.Prefix != "pk" || dest.AccessKey != "ak" || dest.SecretKey != "sk" ||
		!dest.UseSSL || dest.Region != "eu-west-1" {
		t.Fatalf("destination not carried over: %+v", dest)
	}

	// Jobs: linked to their database; oldjob also linked to the destination.
	job, err := store.GetJob(1)
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != "oldjob" || job.DatabaseID != dbOldjob.ID || !job.DestLocal ||
		len(job.DestinationIDs) != 1 || job.DestinationIDs[0] != dest.ID {
		t.Fatalf("oldjob not migrated correctly: %+v", job)
	}
	if n, _ := store.DatabaseJobCount(dbOldjob.ID); n != 1 {
		t.Fatalf("database job count = %d, want 1", n)
	}
	job2, err := store.GetJob(2)
	if err != nil || len(job2.DestinationIDs) != 0 || job2.Schedule != "" {
		t.Fatalf("localonly job wrong: %+v (%v)", job2, err)
	}

	// Backups: stored_s3 replaced by links; oldjob's backup linked.
	b1, err := store.GetBackup(1)
	if err != nil {
		t.Fatal(err)
	}
	bdests, err := store.BackupDestinations(b1.ID)
	if err != nil || len(bdests) != 1 || bdests[0].ID != dest.ID {
		t.Fatalf("backup 1 destinations = %+v (%v), want [s3]", bdests, err)
	}
	b2, _ := store.GetBackup(2)
	if bd, _ := store.BackupDestinations(b2.ID); len(bd) != 0 {
		t.Fatalf("backup 2 should have no destinations, got %+v", bd)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateV2NullableJob builds the current tables by hand except that
// backups.job_id is still NOT NULL, then opens it via Open and asserts the
// migrated shape: old rows keep their job link, the constraint is gone, and
// CreateBackup accepts jobID 0 (stored as NULL, read back as 0).
func TestMigrateV2NullableJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
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
	old.Exec(`CREATE TABLE backups (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  status TEXT NOT NULL,
  trigger TEXT NOT NULL,
  started_at TEXT NOT NULL, finished_at TEXT,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  filename TEXT NOT NULL,
  stored_local INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  restored_at TEXT
)`)
	must := func(_ sql.Result, err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(old.Exec(`INSERT INTO databases (name, host, port, username, password, dbname, created_at, updated_at)
		VALUES ('prof','h1',5432,'u','p','d','t1','t1')`))
	must(old.Exec(`INSERT INTO jobs (name, database_id, created_at, updated_at) VALUES ('oldjob',1,'t1','t1')`))
	must(old.Exec(`INSERT INTO backups (job_id, status, trigger, started_at, filename, stored_local)
		VALUES (1,'completed','cron','s1','oldjob-s1.dump',1)`))
	old.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open+migrate: %v", err)
	}
	defer store.Close()

	// Constraint dropped.
	if notNull, err := store.columnNotNull("backups", "job_id"); err != nil || notNull {
		t.Fatalf("backups.job_id still NOT NULL: %v (%v)", notNull, err)
	}

	// Existing row intact, job link preserved.
	b1, err := store.GetBackup(1)
	if err != nil {
		t.Fatal(err)
	}
	if b1.JobID != 1 || b1.Filename != "oldjob-s1.dump" || b1.Trigger != "cron" {
		t.Fatalf("old backup row not preserved: %+v", b1)
	}

	// Job-less insert succeeds and reads back as JobID 0.
	id, err := store.CreateBackup(0, StatusRunning, "import", Now(), "uploaded-x.dump")
	if err != nil {
		t.Fatalf("create job-less backup: %v", err)
	}
	b2, err := store.GetBackup(id)
	if err != nil {
		t.Fatal(err)
	}
	if b2.JobID != 0 || b2.Filename != "uploaded-x.dump" || b2.Trigger != "import" {
		t.Fatalf("job-less backup wrong: %+v", b2)
	}

	// Job-filtered listing still matches exactly one row.
	list, err := store.ListBackups(1, false)
	if err != nil || len(list) != 1 || list[0].ID != 1 {
		t.Fatalf("ListBackups(1) = %+v (%v), want [backup 1]", list, err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
