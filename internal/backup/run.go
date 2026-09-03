// Package backup is DumpKeeper's engine: triggering pg_dump runs, storing
// results, restoring via pg_restore, and enforcing keep-last-N retention.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"dumpkeeper/internal/db"
	"dumpkeeper/internal/storage"
)

const (
	TriggerManual = "manual"
	TriggerCron   = "cron"

	maxConcurrent = 2    // global pg_dump concurrency
	stderrTailMax = 2048 // stderr tail kept in backups.error
)

// ErrAlreadyRunning is returned by Trigger when the job already has a
// backup in flight.
var ErrAlreadyRunning = errors.New("a backup is already running for this job")

// Engine coordinates backups across the store and destinations.
type Engine struct {
	DB    *db.Store
	Local *storage.Local
	S3    *storage.S3

	sema    chan struct{} // caps concurrent pg_dump runs
	running sync.Map      // jobID -> struct{} while a backup is in flight
	wg      sync.WaitGroup
}

// New builds an Engine. s3 may be nil only for tests that never use S3.
func New(store *db.Store, local *storage.Local, s3 *storage.S3) *Engine {
	return &Engine{
		DB:    store,
		Local: local,
		S3:    s3,
		sema:  make(chan struct{}, maxConcurrent),
	}
}

// Trigger starts RunBackup for the job in a goroutine and returns
// immediately. It returns ErrAlreadyRunning when the job already has a
// backup in flight. Both the manual handler and the scheduler call this.
func (e *Engine) Trigger(jobID int64, trigger string) error {
	if _, loaded := e.running.LoadOrStore(jobID, struct{}{}); loaded {
		return ErrAlreadyRunning
	}
	job, err := e.DB.GetJob(jobID)
	if err != nil {
		e.running.Delete(jobID)
		return fmt.Errorf("load job %d: %w", jobID, err)
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer e.running.Delete(job.ID)
		if err := e.RunBackup(context.Background(), job, trigger); err != nil {
			slog.Error("backup failed", "job", job.Name, "trigger", trigger, "err", err)
		}
	}()
	return nil
}

// Wait blocks until every in-flight backup goroutine has finished. Shutdown
// calls it without a timeout: killing pg_dump mid-write risks a corrupt dump.
func (e *Engine) Wait() { e.wg.Wait() }

// RunBackup performs one full backup of job: pg_dump to a temp file, then
// storage to the configured destinations. Partial success (one destination
// up) counts as completed with the failure noted in backups.error.
func (e *Engine) RunBackup(ctx context.Context, job db.Job, trigger string) error {
	started := time.Now().UTC()
	filename := fmt.Sprintf("%s-%s.dump", job.Name, started.Format("20060102T150405Z"))
	id, err := e.DB.CreateBackup(job.ID, db.StatusRunning, trigger, db.FormatTime(started), filename)
	if err != nil {
		return fmt.Errorf("record backup: %w", err)
	}

	e.sema <- struct{}{}
	defer func() { <-e.sema }()

	tmp, err := os.CreateTemp("", "dk-*.dump")
	if err != nil {
		return e.finishFailed(id, fmt.Sprintf("create temp file: %v", err))
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath) // no-op when local.Put already renamed it away

	if err := runDump(ctx, job, tmpPath); err != nil {
		return e.finishFailed(id, err.Error())
	}

	info, err := os.Stat(tmpPath)
	if err != nil {
		return e.finishFailed(id, fmt.Sprintf("stat dump: %v", err))
	}

	var warns []string
	storedLocal, storedS3 := false, false
	// S3 first: it only reads the temp file, while Local.Put renames it
	// away. The other order would make a local+S3 job fail its S3 leg with
	// "no such file or directory".
	if job.DestS3 {
		if err := e.S3.Put(ctx, filename, tmpPath); err != nil {
			warns = append(warns, "s3: "+err.Error())
		} else {
			storedS3 = true
		}
	}
	if job.DestLocal {
		if err := e.Local.Put(ctx, filename, tmpPath); err != nil {
			warns = append(warns, "local: "+err.Error())
		} else {
			storedLocal = true
		}
	}

	status, errMsg := db.StatusCompleted, strings.Join(warns, "; ")
	if !storedLocal && !storedS3 {
		status = db.StatusFailed
		if errMsg == "" {
			errMsg = "backup stored nowhere: all destinations failed"
		}
	}

	finished := db.Now()
	if err := e.DB.UpdateBackup(db.Backup{
		ID: id, Status: status, FinishedAt: &finished, SizeBytes: info.Size(),
		StoredLocal: storedLocal, StoredS3: storedS3, Error: errMsg,
	}); err != nil {
		slog.Error("backup: update row", "id", id, "err", err)
	}
	slog.Info("backup finished", "job", job.Name, "status", status, "size", info.Size(),
		"local", storedLocal, "s3", storedS3, "warn", errMsg)

	if status == db.StatusCompleted {
		e.Prune(job)
	}
	if status == db.StatusFailed {
		return errors.New(errMsg)
	}
	return nil
}

// finishFailed marks the row failed and returns the error for the caller.
func (e *Engine) finishFailed(id int64, msg string) error {
	finished := db.Now()
	if err := e.DB.UpdateBackup(db.Backup{ID: id, Status: db.StatusFailed, FinishedAt: &finished, Error: msg}); err != nil {
		slog.Error("backup: update row", "id", id, "err", err)
	}
	return errors.New(msg)
}

// stderrTail returns the last max bytes of s, trimmed.
func stderrTail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}
