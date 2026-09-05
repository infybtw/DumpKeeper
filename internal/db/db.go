// Package db is DumpKeeper's SQLite persistence layer: schema, databases,
// destinations, jobs, backups (executions), and sessions. Every query the
// app needs lives here.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// TimeFormat is the fixed-width RFC3339 layout used for every timestamp
// stored in SQLite (always UTC). The fixed fraction width keeps
// lexicographic ordering equal to chronological ordering, so timestamps can
// be compared and sorted as plain strings.
const TimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Backup status values stored in backups.status.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("record not found")

// FormatTime renders t as the canonical storage format (UTC).
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// Now is FormatTime(time.Now()).
func Now() string { return FormatTime(time.Now()) }

// ParseTime parses a stored timestamp.
func ParseTime(s string) (time.Time, error) { return time.Parse(TimeFormat, s) }

// Database is one PostgreSQL connection profile, reusable across jobs.
type Database struct {
	ID        int64
	Name      string
	Host      string
	Port      int
	Username  string
	Password  string
	DBName    string
	SSLMode   string
	CreatedAt string
	UpdatedAt string
}

// Destination is one S3-compatible backup target, reusable across jobs.
type Destination struct {
	ID        int64
	Name      string
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	CreatedAt string
	UpdatedAt string
}

// Job targets one Database; results go to local storage and/or the linked
// Destinations.
type Job struct {
	ID             int64
	Name           string
	DatabaseID     int64
	Schedule       string // cron expression, "" = manual only
	DestLocal      bool
	KeepLast       int64 // 0 = unlimited
	DestinationIDs []int64
	CreatedAt      string
	UpdatedAt      string
}

// Backup is one execution of a job.
type Backup struct {
	ID          int64
	JobID       int64
	Status      string // running | completed | failed
	Trigger     string // manual | cron
	StartedAt   string
	FinishedAt  *string
	SizeBytes   int64
	Filename    string
	StoredLocal bool
	Error       string
	RestoredAt  *string
}

// Session is one authenticated UI session.
type Session struct {
	Token     string
	CSRF      string
	CreatedAt string
	ExpiresAt string
}

// BackupLink says that a backup file is stored on a destination.
type BackupLink struct {
	BackupID    int64
	Destination Destination
}

// Store wraps the SQLite database.
type Store struct {
	sql *sql.DB
}

// ddl is applied at Open; every statement is idempotent. Order matters for
// readability only; SQLite accepts forward references between tables.
var ddl = []string{
	`CREATE TABLE IF NOT EXISTS databases (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 5432,
  username TEXT NOT NULL, password TEXT NOT NULL,
  dbname TEXT NOT NULL, sslmode TEXT NOT NULL DEFAULT 'prefer',
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS destinations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  endpoint TEXT NOT NULL, region TEXT NOT NULL DEFAULT '',
  bucket TEXT NOT NULL, prefix TEXT NOT NULL DEFAULT '',
  access_key TEXT NOT NULL, secret_key TEXT NOT NULL,
  use_ssl INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  database_id INTEGER NOT NULL REFERENCES databases(id),
  schedule TEXT NOT NULL DEFAULT '',
  dest_local INTEGER NOT NULL DEFAULT 1,
  keep_last INTEGER NOT NULL DEFAULT 7,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS job_destinations (
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  destination_id INTEGER NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
  PRIMARY KEY (job_id, destination_id)
)`,
	`CREATE TABLE IF NOT EXISTS backups (
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
)`,
	`CREATE INDEX IF NOT EXISTS idx_backups_job ON backups(job_id, started_at DESC)`,
	`CREATE TABLE IF NOT EXISTS backup_destinations (
  backup_id INTEGER NOT NULL REFERENCES backups(id) ON DELETE CASCADE,
  destination_id INTEGER NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
  PRIMARY KEY (backup_id, destination_id)
)`,
	`CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY, value TEXT NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS ping_state (
  database_id INTEGER PRIMARY KEY REFERENCES databases(id) ON DELETE CASCADE,
  ok INTEGER NOT NULL, checked_at TEXT NOT NULL,
  duration_ms INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT ''
)`,
	`CREATE TABLE IF NOT EXISTS ping_incidents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  database_id INTEGER NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
  started_at TEXT NOT NULL, ended_at TEXT,
  error TEXT NOT NULL DEFAULT ''
)`,
	`CREATE INDEX IF NOT EXISTS idx_ping_incidents_db ON ping_incidents(database_id, started_at DESC)`,
	`CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY, csrf TEXT NOT NULL,
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL
)`,
}

// Open opens (creating if needed) the database at path, applies the schema,
// and migrates pre-2.0 layouts. foreign_keys is enabled via DSN so every
// pooled connection gets it.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys=1&_pragma=busy_timeout=10000&_pragma=journal_mode=WAL"
	sq, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One connection for in-memory databases: each pooled connection would
	// otherwise get its own empty :memory: schema.
	if path == ":memory:" {
		sq.SetMaxOpenConns(1)
	}
	for _, stmt := range ddl {
		if _, err := sq.Exec(stmt); err != nil {
			sq.Close()
			return nil, fmt.Errorf("apply schema: %w", err)
		}
	}
	s := &Store{sql: sq}
	if err := s.migrateV1(); err != nil {
		sq.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.sql.Close() }

// migrateV1 reshapes the MVP layout in place: jobs used to embed database
// credentials and S3 was one global setting. Detected via the legacy
// jobs.dbname column; a no-op on fresh databases. A database entity is
// created per old job (same name), and the global S3 settings become a
// single destination named "s3" linked to the jobs that used them.
func (s *Store) migrateV1() error {
	old, err := s.tableHasColumn("jobs", "dbname")
	if err != nil || !old {
		return err
	}

	tx, err := s.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()

	// 1. Global S3 settings become one destination.
	settings := map[string]string{}
	srows, err := tx.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return fmt.Errorf("read legacy settings: %w", err)
	}
	for srows.Next() {
		var k, v string
		if err := srows.Scan(&k, &v); err != nil {
			srows.Close()
			return err
		}
		settings[k] = v
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return err
	}
	var destID int64
	if settings["s3_endpoint"] != "" {
		useSSL := "0"
		if settings["s3_use_ssl"] == "1" {
			useSSL = "1"
		}
		res, err := tx.Exec(
			`INSERT INTO destinations (name, endpoint, region, bucket, prefix, access_key, secret_key, use_ssl, created_at, updated_at)
			 VALUES ('s3', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			settings["s3_endpoint"], settings["s3_region"], settings["s3_bucket"], settings["s3_prefix"],
			settings["s3_access_key"], settings["s3_secret_key"], useSSL, Now(), Now())
		if err != nil {
			return fmt.Errorf("migrate destination: %w", err)
		}
		destID, _ = res.LastInsertId()
	}

	// 2. One database entity per job (job names are unique, so these are too).
	if _, err := tx.Exec(
		`INSERT INTO databases (name, host, port, username, password, dbname, sslmode, created_at, updated_at)
		 SELECT name, host, port, username, password, dbname, sslmode, created_at, updated_at FROM jobs`); err != nil {
		return fmt.Errorf("migrate databases: %w", err)
	}

	// 3. Point jobs at their new database entity.
	if _, err := tx.Exec(`ALTER TABLE jobs ADD COLUMN database_id INTEGER REFERENCES databases(id)`); err != nil {
		return fmt.Errorf("migrate jobs: %w", err)
	}
	if _, err := tx.Exec(`UPDATE jobs SET database_id = (SELECT id FROM databases WHERE name = jobs.name)`); err != nil {
		return fmt.Errorf("migrate jobs: %w", err)
	}

	// 4. Move the S3 flag to destination links; jobs left with no
	// destination fall back to local so they never become unrunnable.
	if destID > 0 {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO job_destinations (job_id, destination_id) SELECT id, ? FROM jobs WHERE dest_s3 = 1`, destID); err != nil {
			return fmt.Errorf("migrate job destinations: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO backup_destinations (backup_id, destination_id) SELECT id, ? FROM backups WHERE stored_s3 = 1`, destID); err != nil {
			return fmt.Errorf("migrate backup destinations: %w", err)
		}
	} else if _, err := tx.Exec(`UPDATE jobs SET dest_local = 1 WHERE dest_s3 = 1`); err != nil {
		return fmt.Errorf("migrate job destinations: %w", err)
	}

	// 5. Drop legacy columns and the replaced settings table.
	for _, col := range []string{"host", "port", "username", "password", "dbname", "sslmode", "dest_s3"} {
		if _, err := tx.Exec(`ALTER TABLE jobs DROP COLUMN ` + col); err != nil {
			return fmt.Errorf("migrate jobs: drop %s: %w", col, err)
		}
	}
	if _, err := tx.Exec(`ALTER TABLE backups DROP COLUMN stored_s3`); err != nil {
		return fmt.Errorf("migrate backups: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE settings`); err != nil {
		return fmt.Errorf("migrate settings: %w", err)
	}
	return tx.Commit()
}

// tableHasColumn reports whether table has a column with the given name.
func (s *Store) tableHasColumn(table, column string) (bool, error) {
	rows, err := s.sql.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- databases ----

const databaseCols = "id, name, host, port, username, password, dbname, sslmode, created_at, updated_at"

func scanDatabase(row interface{ Scan(dest ...any) error }) (Database, error) {
	var d Database
	err := row.Scan(&d.ID, &d.Name, &d.Host, &d.Port, &d.Username, &d.Password, &d.DBName, &d.SSLMode, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

// CreateDatabase inserts d, fills its timestamps, and returns it with the new ID.
func (s *Store) CreateDatabase(d Database) (Database, error) {
	now := Now()
	res, err := s.sql.Exec(
		`INSERT INTO databases (name, host, port, username, password, dbname, sslmode, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		d.Name, d.Host, d.Port, d.Username, d.Password, d.DBName, d.SSLMode, now, now)
	if err != nil {
		return Database{}, fmt.Errorf("create database: %w", err)
	}
	d.ID, _ = res.LastInsertId()
	d.CreatedAt, d.UpdatedAt = now, now
	return d, nil
}

// UpdateDatabase updates all editable fields and bumps updated_at.
func (s *Store) UpdateDatabase(d Database) error {
	_, err := s.sql.Exec(
		`UPDATE databases SET name=?, host=?, port=?, username=?, password=?, dbname=?, sslmode=?, updated_at=? WHERE id=?`,
		d.Name, d.Host, d.Port, d.Username, d.Password, d.DBName, d.SSLMode, Now(), d.ID)
	if err != nil {
		return fmt.Errorf("update database %d: %w", d.ID, err)
	}
	return nil
}

// DeleteDatabase removes a database. The caller must refuse when jobs
// reference it (no FK cascade by design: dropping a database silently
// deletes its jobs would be a surprise).
func (s *Store) DeleteDatabase(id int64) error {
	_, err := s.sql.Exec(`DELETE FROM databases WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete database %d: %w", id, err)
	}
	return nil
}

// GetDatabase returns one database or ErrNotFound.
func (s *Store) GetDatabase(id int64) (Database, error) {
	d, err := scanDatabase(s.sql.QueryRow(`SELECT `+databaseCols+` FROM databases WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Database{}, ErrNotFound
	}
	return d, err
}

// ListDatabases returns all databases ordered by name.
func (s *Store) ListDatabases() ([]Database, error) {
	return s.listDatabasesQuery(`SELECT ` + databaseCols + ` FROM databases ORDER BY name`)
}

func (s *Store) listDatabasesQuery(q string, args ...any) ([]Database, error) {
	rows, err := s.sql.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	defer rows.Close()
	var out []Database
	for rows.Next() {
		d, err := scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DatabaseJobCount counts jobs referencing a database.
func (s *Store) DatabaseJobCount(id int64) (int, error) {
	var n int
	err := s.sql.QueryRow(`SELECT COUNT(1) FROM jobs WHERE database_id=?`, id).Scan(&n)
	return n, err
}

// DatabaseNameExists reports whether another database (id != excludeID)
// already uses name.
func (s *Store) DatabaseNameExists(name string, excludeID int64) (bool, error) {
	var n int
	err := s.sql.QueryRow(`SELECT COUNT(1) FROM databases WHERE name=? AND id != ?`, name, excludeID).Scan(&n)
	return n > 0, err
}

// ---- destinations ----

const destinationCols = "id, name, endpoint, region, bucket, prefix, access_key, secret_key, use_ssl, created_at, updated_at"

const destinationColsQualified = "d.id, d.name, d.endpoint, d.region, d.bucket, d.prefix, d.access_key, d.secret_key, d.use_ssl, d.created_at, d.updated_at"

func scanDestination(row interface{ Scan(dest ...any) error }) (Destination, error) {
	var d Destination
	var useSSL int
	err := row.Scan(&d.ID, &d.Name, &d.Endpoint, &d.Region, &d.Bucket, &d.Prefix, &d.AccessKey, &d.SecretKey, &useSSL, &d.CreatedAt, &d.UpdatedAt)
	d.UseSSL = useSSL == 1
	return d, err
}

// CreateDestination inserts d, fills its timestamps, and returns it with the new ID.
func (s *Store) CreateDestination(d Destination) (Destination, error) {
	now := Now()
	res, err := s.sql.Exec(
		`INSERT INTO destinations (name, endpoint, region, bucket, prefix, access_key, secret_key, use_ssl, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		d.Name, d.Endpoint, d.Region, d.Bucket, d.Prefix, d.AccessKey, d.SecretKey, b2i(d.UseSSL), now, now)
	if err != nil {
		return Destination{}, fmt.Errorf("create destination: %w", err)
	}
	d.ID, _ = res.LastInsertId()
	d.CreatedAt, d.UpdatedAt = now, now
	return d, nil
}

// UpdateDestination updates all editable fields and bumps updated_at.
func (s *Store) UpdateDestination(d Destination) error {
	_, err := s.sql.Exec(
		`UPDATE destinations SET name=?, endpoint=?, region=?, bucket=?, prefix=?, access_key=?, secret_key=?, use_ssl=?, updated_at=? WHERE id=?`,
		d.Name, d.Endpoint, d.Region, d.Bucket, d.Prefix, d.AccessKey, d.SecretKey, b2i(d.UseSSL), Now(), d.ID)
	if err != nil {
		return fmt.Errorf("update destination %d: %w", d.ID, err)
	}
	return nil
}

// DeleteDestination removes a destination; job and backup links cascade.
func (s *Store) DeleteDestination(id int64) error {
	_, err := s.sql.Exec(`DELETE FROM destinations WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete destination %d: %w", id, err)
	}
	return nil
}

// GetDestination returns one destination or ErrNotFound.
func (s *Store) GetDestination(id int64) (Destination, error) {
	d, err := scanDestination(s.sql.QueryRow(`SELECT `+destinationCols+` FROM destinations WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Destination{}, ErrNotFound
	}
	return d, err
}

// ListDestinations returns all destinations ordered by name.
func (s *Store) ListDestinations() ([]Destination, error) {
	rows, err := s.sql.Query(`SELECT ` + destinationCols + ` FROM destinations ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list destinations: %w", err)
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DestinationNameExists reports whether another destination (id != excludeID)
// already uses name.
func (s *Store) DestinationNameExists(name string, excludeID int64) (bool, error) {
	var n int
	err := s.sql.QueryRow(`SELECT COUNT(1) FROM destinations WHERE name=? AND id != ?`, name, excludeID).Scan(&n)
	return n > 0, err
}

// ---- jobs ----

const jobCols = "id, name, database_id, schedule, dest_local, keep_last, created_at, updated_at"

func scanJob(row interface{ Scan(dest ...any) error }) (Job, error) {
	var j Job
	var destLocal int
	err := row.Scan(&j.ID, &j.Name, &j.DatabaseID, &j.Schedule, &destLocal, &j.KeepLast, &j.CreatedAt, &j.UpdatedAt)
	j.DestLocal = destLocal == 1
	return j, err
}

// jobDestinationIDs loads an job's S3 destination links.
func (s *Store) jobDestinationIDs(jobID int64) ([]int64, error) {
	rows, err := s.sql.Query(`SELECT destination_id FROM job_destinations WHERE job_id=? ORDER BY destination_id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("job destinations: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CreateJob inserts j with its destination links, fills timestamps, and
// returns it with the new ID.
func (s *Store) CreateJob(j Job) (Job, error) {
	now := Now()
	tx, err := s.sql.Begin()
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`INSERT INTO jobs (name, database_id, schedule, dest_local, keep_last, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
		j.Name, j.DatabaseID, j.Schedule, b2i(j.DestLocal), j.KeepLast, now, now)
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	j.ID, _ = res.LastInsertId()
	for _, did := range j.DestinationIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO job_destinations (job_id, destination_id) VALUES (?,?)`, j.ID, did); err != nil {
			return Job{}, fmt.Errorf("create job links: %w", err)
		}
	}
	j.CreatedAt, j.UpdatedAt = now, now
	return j, tx.Commit()
}

// UpdateJob updates all editable fields and replaces the destination links.
func (s *Store) UpdateJob(j Job) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return fmt.Errorf("update job: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE jobs SET name=?, database_id=?, schedule=?, dest_local=?, keep_last=?, updated_at=? WHERE id=?`,
		j.Name, j.DatabaseID, j.Schedule, b2i(j.DestLocal), j.KeepLast, Now(), j.ID); err != nil {
		return fmt.Errorf("update job %d: %w", j.ID, err)
	}
	if _, err := tx.Exec(`DELETE FROM job_destinations WHERE job_id=?`, j.ID); err != nil {
		return fmt.Errorf("update job links: %w", err)
	}
	for _, did := range j.DestinationIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO job_destinations (job_id, destination_id) VALUES (?,?)`, j.ID, did); err != nil {
			return fmt.Errorf("update job links: %w", err)
		}
	}
	return tx.Commit()
}

// DeleteJob removes the job; its links and backups cascade.
func (s *Store) DeleteJob(id int64) error {
	_, err := s.sql.Exec(`DELETE FROM jobs WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete job %d: %w", id, err)
	}
	return nil
}

// GetJob returns one job (with destination links) or ErrNotFound.
func (s *Store) GetJob(id int64) (Job, error) {
	j, err := scanJob(s.sql.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	j.DestinationIDs, err = s.jobDestinationIDs(j.ID)
	return j, err
}

// ListJobs returns all jobs (with destination links) ordered by name.
func (s *Store) ListJobs() ([]Job, error) {
	rows, err := s.sql.Query(`SELECT ` + jobCols + ` FROM jobs ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range jobs {
		if jobs[i].DestinationIDs, err = s.jobDestinationIDs(jobs[i].ID); err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

// JobDestinations returns the S3 destinations a job stores to.
func (s *Store) JobDestinations(jobID int64) ([]Destination, error) {
	rows, err := s.sql.Query(
		`SELECT `+destinationColsQualified+` FROM destinations d
		 JOIN job_destinations jd ON jd.destination_id = d.id
		 WHERE jd.job_id = ? ORDER BY d.name`, jobID)
	if err != nil {
		return nil, fmt.Errorf("job destinations: %w", err)
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// JobNameExists reports whether another job (id != excludeID) already uses name.
func (s *Store) JobNameExists(name string, excludeID int64) (bool, error) {
	var n int
	err := s.sql.QueryRow(`SELECT COUNT(1) FROM jobs WHERE name=? AND id != ?`, name, excludeID).Scan(&n)
	return n > 0, err
}

// ---- backups (executions) ----

const backupCols = `id, job_id, status, "trigger", started_at, finished_at, size_bytes, filename, stored_local, error, restored_at`

func scanBackup(row interface{ Scan(dest ...any) error }) (Backup, error) {
	var b Backup
	var storedLocal int
	var finished, restored sql.NullString
	err := row.Scan(&b.ID, &b.JobID, &b.Status, &b.Trigger, &b.StartedAt, &finished,
		&b.SizeBytes, &b.Filename, &storedLocal, &b.Error, &restored)
	b.StoredLocal = storedLocal == 1
	if finished.Valid {
		b.FinishedAt = &finished.String
	}
	if restored.Valid {
		b.RestoredAt = &restored.String
	}
	return b, err
}

// CreateBackup inserts a new backups row and returns its ID.
func (s *Store) CreateBackup(jobID int64, status, trigger, startedAt, filename string) (int64, error) {
	res, err := s.sql.Exec(
		`INSERT INTO backups (job_id, status, "trigger", started_at, filename) VALUES (?,?,?,?,?)`,
		jobID, status, trigger, startedAt, filename)
	if err != nil {
		return 0, fmt.Errorf("create backup: %w", err)
	}
	return res.LastInsertId()
}
func (s *Store) UpdateBackup(b Backup) error {
	finished := sql.NullString{Valid: b.FinishedAt != nil}
	if b.FinishedAt != nil {
		finished.String = *b.FinishedAt
	}
	restored := sql.NullString{Valid: b.RestoredAt != nil}
	if b.RestoredAt != nil {
		restored.String = *b.RestoredAt
	}
	_, err := s.sql.Exec(
		`UPDATE backups SET status=?, finished_at=?, size_bytes=?, stored_local=?, error=?, restored_at=? WHERE id=?`,
		b.Status, finished, b.SizeBytes, b2i(b.StoredLocal), b.Error, restored, b.ID)
	if err != nil {
		return fmt.Errorf("update backup %d: %w", b.ID, err)
	}
	return nil
}

// MarkBackupDestination records that a backup file is stored on an S3 destination.
func (s *Store) MarkBackupDestination(backupID, destinationID int64) error {
	_, err := s.sql.Exec(`INSERT OR IGNORE INTO backup_destinations (backup_id, destination_id) VALUES (?,?)`, backupID, destinationID)
	if err != nil {
		return fmt.Errorf("mark backup destination: %w", err)
	}
	return nil
}

// BackupDestinations returns the S3 destinations holding a backup's file.
func (s *Store) BackupDestinations(backupID int64) ([]Destination, error) {
	rows, err := s.sql.Query(
		`SELECT `+destinationColsQualified+` FROM destinations d
		 JOIN backup_destinations bd ON bd.destination_id = d.id
		 WHERE bd.backup_id = ? ORDER BY d.name`, backupID)
	if err != nil {
		return nil, fmt.Errorf("backup destinations: %w", err)
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListBackupLinks returns every backup -> destination link with the
// destination resolved, for rendering stored-on chips.
func (s *Store) ListBackupLinks() ([]BackupLink, error) {
	rows, err := s.sql.Query(
		`SELECT bd.backup_id, ` + destinationColsQualified + ` FROM destinations d
		 JOIN backup_destinations bd ON bd.destination_id = d.id`)
	if err != nil {
		return nil, fmt.Errorf("list backup links: %w", err)
	}
	defer rows.Close()
	var out []BackupLink
	for rows.Next() {
		var l BackupLink
		var d Destination
		var useSSL int
		if err := rows.Scan(&l.BackupID, &d.ID, &d.Name, &d.Endpoint, &d.Region, &d.Bucket,
			&d.Prefix, &d.AccessKey, &d.SecretKey, &useSSL, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.UseSSL = useSSL == 1
		l.Destination = d
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetBackup returns one backup or ErrNotFound.
func (s *Store) GetBackup(id int64) (Backup, error) {
	b, err := scanBackup(s.sql.QueryRow(`SELECT `+backupCols+` FROM backups WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Backup{}, ErrNotFound
	}
	return b, err
}

// ListBackups returns backups, newest first. jobID 0 means all jobs;
// onlyCompleted filters to status 'completed'.
func (s *Store) ListBackups(jobID int64, onlyCompleted bool) ([]Backup, error) {
	q := `SELECT ` + backupCols + ` FROM backups`
	var where []string
	var args []any
	if jobID > 0 {
		where = append(where, `job_id = ?`)
		args = append(args, jobID)
	}
	if onlyCompleted {
		where = append(where, `status = ?`)
		args = append(args, StatusCompleted)
	}
	if len(where) > 0 {
		q += ` WHERE ` + where[0]
		for _, w := range where[1:] {
			q += ` AND ` + w
		}
	}
	q += ` ORDER BY started_at DESC, id DESC`
	rows, err := s.sql.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListBackupsForDestination returns the backups stored on a destination,
// for object cleanup when the destination is deleted.
func (s *Store) ListBackupsForDestination(destinationID int64) ([]Backup, error) {
	rows, err := s.sql.Query(
		`SELECT `+backupCols+` FROM backups
		 JOIN backup_destinations bd ON bd.backup_id = backups.id
		 WHERE bd.destination_id = ?`, destinationID)
	if err != nil {
		return nil, fmt.Errorf("backups for destination: %w", err)
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LatestBackups returns the most recent backup per job, keyed by job ID.
func (s *Store) LatestBackups() (map[int64]Backup, error) {
	rows, err := s.sql.Query(
		`SELECT ` + backupCols + ` FROM backups
		 WHERE id IN (SELECT MAX(id) FROM backups GROUP BY job_id)`)
	if err != nil {
		return nil, fmt.Errorf("latest backups: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]Backup)
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out[b.JobID] = b
	}
	return out, rows.Err()
}

// DeleteBackup removes a single backups row (destination links cascade).
func (s *Store) DeleteBackup(id int64) error {
	_, err := s.sql.Exec(`DELETE FROM backups WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete backup %d: %w", id, err)
	}
	return nil
}

// ---- sessions ----

// CreateSession stores a new session with the given expiry.
func (s *Store) CreateSession(token, csrf, expiresAt string) error {
	_, err := s.sql.Exec(
		`INSERT INTO sessions (token, csrf, created_at, expires_at) VALUES (?,?,?,?)`,
		token, csrf, Now(), expiresAt)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession returns the session for token if it exists and is unexpired.
func (s *Store) GetSession(token string) (Session, error) {
	var sess Session
	err := s.sql.QueryRow(
		`SELECT token, csrf, created_at, expires_at FROM sessions WHERE token=? AND expires_at > ?`,
		token, Now()).Scan(&sess.Token, &sess.CSRF, &sess.CreatedAt, &sess.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return sess, nil
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(token string) error {
	_, err := s.sql.Exec(`DELETE FROM sessions WHERE token=?`, token)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions purges sessions past their expiry.
func (s *Store) DeleteExpiredSessions() error {
	_, err := s.sql.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, Now())
	if err != nil {
		return fmt.Errorf("delete expired sessions: %w", err)
	}
	return nil
}

// ---- availability monitoring ----

// SettingPingInterval stores the availability probe interval in seconds;
// "0" disables monitoring.
const SettingPingInterval = "ping_interval_seconds"

// PingState is the latest availability probe result for a database.
type PingState struct {
	DatabaseID int64
	OK         bool
	CheckedAt  string
	DurationMs int64
	Error      string
}

// Incident is one downtime period: from the first failed ping to the first
// success after it. EndedAt is nil while the database is still down.
type Incident struct {
	ID         int64
	DatabaseID int64
	DBName     string
	StartedAt  string
	EndedAt    *string
	Error      string
}

// GetSetting returns the raw value for key or ErrNotFound.
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.sql.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get setting %s: %w", key, err)
	}
	return v, nil
}

// SetSetting inserts or updates a settings row.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.sql.Exec(
		`INSERT INTO settings (key, value) VALUES (?,?)
  ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// UpsertPingState records the latest probe result for a database.
func (s *Store) UpsertPingState(st PingState) error {
	_, err := s.sql.Exec(
		`INSERT INTO ping_state (database_id, ok, checked_at, duration_ms, error) VALUES (?,?,?,?,?)
  ON CONFLICT(database_id) DO UPDATE SET
    ok=excluded.ok, checked_at=excluded.checked_at,
    duration_ms=excluded.duration_ms, error=excluded.error`,
		st.DatabaseID, b2i(st.OK), st.CheckedAt, st.DurationMs, st.Error)
	if err != nil {
		return fmt.Errorf("upsert ping state: %w", err)
	}
	return nil
}

// ListPingStates returns the latest probe result per database, keyed by ID.
func (s *Store) ListPingStates() (map[int64]PingState, error) {
	rows, err := s.sql.Query(`SELECT database_id, ok, checked_at, duration_ms, error FROM ping_state`)
	if err != nil {
		return nil, fmt.Errorf("list ping states: %w", err)
	}
	defer rows.Close()
	states := map[int64]PingState{}
	for rows.Next() {
		var st PingState
		var ok int
		if err := rows.Scan(&st.DatabaseID, &ok, &st.CheckedAt, &st.DurationMs, &st.Error); err != nil {
			return nil, fmt.Errorf("scan ping state: %w", err)
		}
		st.OK = ok == 1
		states[st.DatabaseID] = st
	}
	return states, rows.Err()
}

// OpenIncident records the start of a downtime period.
func (s *Store) OpenIncident(databaseID int64, startedAt, errMsg string) error {
	_, err := s.sql.Exec(
		`INSERT INTO ping_incidents (database_id, started_at, error) VALUES (?,?,?)`,
		databaseID, startedAt, errMsg)
	if err != nil {
		return fmt.Errorf("open incident: %w", err)
	}
	return nil
}

// CloseOpenIncident ends the still-open downtime period of a database.
func (s *Store) CloseOpenIncident(databaseID int64, endedAt string) error {
	_, err := s.sql.Exec(
		`UPDATE ping_incidents SET ended_at=? WHERE database_id=? AND ended_at IS NULL`,
		endedAt, databaseID)
	if err != nil {
		return fmt.Errorf("close incident: %w", err)
	}
	return nil
}

// TouchOpenIncident refreshes the last seen error of an ongoing downtime.
func (s *Store) TouchOpenIncident(databaseID int64, errMsg string) error {
	_, err := s.sql.Exec(
		`UPDATE ping_incidents SET error=? WHERE database_id=? AND ended_at IS NULL`,
		errMsg, databaseID)
	if err != nil {
		return fmt.Errorf("touch incident: %w", err)
	}
	return nil
}

// ListIncidents returns downtime periods, ongoing ones first, then newest.
func (s *Store) ListIncidents(limit int64) ([]Incident, error) {
	rows, err := s.sql.Query(
		`SELECT i.id, i.database_id, COALESCE(d.name,''), i.started_at, i.ended_at, i.error
  FROM ping_incidents i LEFT JOIN databases d ON d.id = i.database_id
  ORDER BY (i.ended_at IS NOT NULL), i.started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list incidents: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		var inc Incident
		if err := rows.Scan(&inc.ID, &inc.DatabaseID, &inc.DBName, &inc.StartedAt, &inc.EndedAt, &inc.Error); err != nil {
			return nil, fmt.Errorf("scan incident: %w", err)
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// ---- dashboard stats ----

// CountBackupsByStatus returns the number of backups per status
// (running/completed/failed).
func (s *Store) CountBackupsByStatus() (map[string]int64, error) {
	rows, err := s.sql.Query(`SELECT status, COUNT(*) FROM backups GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("count backups by status: %w", err)
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan backup count: %w", err)
		}
		counts[status] = n
	}
	return counts, rows.Err()
}

// RestorationStats returns how many backups were restored and when the
// last restore happened (nil when never).
func (s *Store) RestorationStats() (int64, *string, error) {
	var n int64
	var last *string
	err := s.sql.QueryRow(
		`SELECT COUNT(*), MAX(restored_at) FROM backups WHERE restored_at IS NOT NULL`,
	).Scan(&n, &last)
	if err != nil {
		return 0, nil, fmt.Errorf("restoration stats: %w", err)
	}
	return n, last, nil
}
