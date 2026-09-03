// Package db is DumpKeeper's SQLite persistence layer: schema, jobs,
// backups, settings, and sessions. Every query the app needs lives here.
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

// Job is one backup target database.
type Job struct {
	ID        int64
	Name      string
	Host      string
	Port      int
	Username  string
	Password  string
	DBName    string
	SSLMode   string
	Schedule  string // cron expression, "" = manual only
	DestLocal bool
	DestS3    bool
	KeepLast  int64 // 0 = unlimited
	CreatedAt string
	UpdatedAt string
}

// Backup is one dump attempt for a job.
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
	StoredS3    bool
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

// Store wraps the SQLite database.
type Store struct {
	sql *sql.DB
}

// ddl is applied at Open; every statement is idempotent.
var ddl = []string{
	`CREATE TABLE IF NOT EXISTS jobs (
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
  stored_s3 INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  restored_at TEXT
)`,
	`CREATE INDEX IF NOT EXISTS idx_backups_job ON backups(job_id, started_at DESC)`,
	`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY, csrf TEXT NOT NULL,
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL
)`,
}

// Open opens (creating if needed) the database at path and applies the
// schema. foreign_keys is enabled via DSN so every pooled connection gets it.
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
	return &Store{sql: sq}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.sql.Close() }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- jobs ----

const jobCols = "id, name, host, port, username, password, dbname, sslmode, schedule, dest_local, dest_s3, keep_last, created_at, updated_at"

func scanJob(row interface{ Scan(dest ...any) error }) (Job, error) {
	var j Job
	var destLocal, destS3 int
	err := row.Scan(&j.ID, &j.Name, &j.Host, &j.Port, &j.Username, &j.Password, &j.DBName,
		&j.SSLMode, &j.Schedule, &destLocal, &destS3, &j.KeepLast, &j.CreatedAt, &j.UpdatedAt)
	j.DestLocal = destLocal == 1
	j.DestS3 = destS3 == 1
	return j, err
}

// CreateJob inserts j, fills its timestamps, and returns it with the new ID.
func (s *Store) CreateJob(j Job) (Job, error) {
	now := Now()
	res, err := s.sql.Exec(
		`INSERT INTO jobs (name, host, port, username, password, dbname, sslmode, schedule, dest_local, dest_s3, keep_last, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.Name, j.Host, j.Port, j.Username, j.Password, j.DBName, j.SSLMode, j.Schedule,
		b2i(j.DestLocal), b2i(j.DestS3), j.KeepLast, now, now)
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	j.ID, _ = res.LastInsertId()
	j.CreatedAt, j.UpdatedAt = now, now
	return j, nil
}

// UpdateJob updates all editable fields of j and bumps updated_at.
func (s *Store) UpdateJob(j Job) error {
	_, err := s.sql.Exec(
		`UPDATE jobs SET name=?, host=?, port=?, username=?, password=?, dbname=?, sslmode=?, schedule=?, dest_local=?, dest_s3=?, keep_last=?, updated_at=? WHERE id=?`,
		j.Name, j.Host, j.Port, j.Username, j.Password, j.DBName, j.SSLMode, j.Schedule,
		b2i(j.DestLocal), b2i(j.DestS3), j.KeepLast, Now(), j.ID)
	if err != nil {
		return fmt.Errorf("update job %d: %w", j.ID, err)
	}
	return nil
}

// DeleteJob removes the job; its backups cascade.
func (s *Store) DeleteJob(id int64) error {
	_, err := s.sql.Exec(`DELETE FROM jobs WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete job %d: %w", id, err)
	}
	return nil
}

// GetJob returns one job or ErrNotFound.
func (s *Store) GetJob(id int64) (Job, error) {
	j, err := scanJob(s.sql.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

// ListJobs returns all jobs ordered by name.
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
	return jobs, rows.Err()
}

// JobNameExists reports whether another job (id != excludeID) already uses name.
func (s *Store) JobNameExists(name string, excludeID int64) (bool, error) {
	var n int
	err := s.sql.QueryRow(`SELECT COUNT(1) FROM jobs WHERE name=? AND id != ?`, name, excludeID).Scan(&n)
	return n > 0, err
}

// ---- backups ----

const backupCols = `id, job_id, status, "trigger", started_at, finished_at, size_bytes, filename, stored_local, stored_s3, error, restored_at`

func scanBackup(row interface{ Scan(dest ...any) error }) (Backup, error) {
	var b Backup
	var storedLocal, storedS3 int
	var finished, restored sql.NullString
	err := row.Scan(&b.ID, &b.JobID, &b.Status, &b.Trigger, &b.StartedAt, &finished,
		&b.SizeBytes, &b.Filename, &storedLocal, &storedS3, &b.Error, &restored)
	b.StoredLocal = storedLocal == 1
	b.StoredS3 = storedS3 == 1
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

// UpdateBackup rewrites the mutable columns of the row with b.ID.
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
		`UPDATE backups SET status=?, finished_at=?, size_bytes=?, stored_local=?, stored_s3=?, error=?, restored_at=? WHERE id=?`,
		b.Status, finished, b.SizeBytes, b2i(b.StoredLocal), b2i(b.StoredS3), b.Error, restored, b.ID)
	if err != nil {
		return fmt.Errorf("update backup %d: %w", b.ID, err)
	}
	return nil
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

// LatestBackups returns the most recent backup per job, keyed by job ID.
func (s *Store) LatestBackups() (map[int64]Backup, error) {
	rows, err := s.sql.Query(
		`SELECT ` + backupCols + ` FROM backups
		 WHERE id IN (SELECT MAX(id) FROM backups GROUP BY job_id)`)
	if err != nil {
		return nil, fmt.Errorf("latest backups: %w", err)
	}
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

// DeleteBackup removes a single backups row.
func (s *Store) DeleteBackup(id int64) error {
	_, err := s.sql.Exec(`DELETE FROM backups WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete backup %d: %w", id, err)
	}
	return nil
}

// ---- settings ----

// GetSettings returns every settings key/value pair.
func (s *Store) GetSettings() (map[string]string, error) {
	rows, err := s.sql.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("get settings: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SaveSettings upserts every pair in m.
func (s *Store) SaveSettings(m map[string]string) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	defer tx.Rollback()
	for k, v := range m {
		if _, err := tx.Exec(
			`INSERT INTO settings (key, value) VALUES (?,?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return fmt.Errorf("save setting %s: %w", k, err)
		}
	}
	return tx.Commit()
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
