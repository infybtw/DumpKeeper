package backup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"dumpkeeper/internal/db"
)

// unsafeName matches every run of characters that does not belong in a
// backup filename.
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeFilename maps every unsafe run to a single underscore. The
// database name is user input, so this both keeps generated filenames
// filesystem-safe and blocks path traversal via filepath.Join.
func sanitizeFilename(s string) string { return unsafeName.ReplaceAllString(s, "_") }

// StartImport records an import row, spools the uploaded dump into the local
// backup directory, and starts the restore — optionally after a safety dump
// of the target database — in a goroutine. It returns immediately; live
// status shows up in Executions. Imports are serialized engine-wide: a
// second concurrent import is rejected instead of racing pg_restore.
func (e *Engine) StartImport(dbe db.Database, src io.Reader, backupFirst bool) (int64, string, error) {
	if !e.importMu.TryLock() {
		return 0, "", errors.New("another import or restore is already running")
	}
	filename := sanitizeFilename(dbe.DBName) + "-uploaded-" + time.Now().UTC().Format("20060102T150405Z") + ".dump"
	id, err := e.DB.CreateBackup(0, db.StatusRunning, TriggerImport, db.Now(), filename)
	if err != nil {
		e.importMu.Unlock()
		return 0, "", errors.New("record import: " + err.Error())
	}

	dst, err := os.Create(filepath.Join(e.Local.Root, filename))
	if err == nil {
		_, err = io.Copy(dst, src)
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		os.Remove(filepath.Join(e.Local.Root, filename)) // partial upload
		e.importMu.Unlock()
		return id, filename, e.finishFailed(id, "spool upload: "+err.Error())
	}

	// From here the goroutine owns the import slot: released when the
	// restore finishes. wg ties it into Engine.Wait so shutdown never kills
	// pg_restore mid-write.
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer e.importMu.Unlock()
		e.importRun(id, dbe, filename, backupFirst)
	}()
	return id, filename, nil
}

// Deliberate choices: an import never deletes the uploaded file, not even
// on failure — it is the user's only copy, stays downloadable from
// Executions, and Delete removes file and row together. Imports and safety
// dumps are local-only: with no job there are no S3 destinations, and
// per-job retention never prunes job-less rows, so nothing silently
// removes them.

// importRun performs the uploaded file's restore, optionally after a safety
// dump of the target database. A failed safety dump aborts the import: the
// user asked to preserve the current state, and dropping objects after a
// skipped safety dump would be worse than not restoring.
func (e *Engine) importRun(id int64, dbe db.Database, filename string, backupFirst bool) {
	ctx := context.Background()

	if backupFirst {
		safetyName := sanitizeFilename(dbe.DBName) + "-pre-restore-" + time.Now().UTC().Format("20060102T150405Z") + ".dump"
		tmp, err := os.CreateTemp("", "dk-*.dump")
		if err != nil {
			e.finishImportFailed(id, "create temp file: "+err.Error(), true)
			return
		}
		tmpPath := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpPath) // no-op once Local.Put renamed it away
		if err := runDump(ctx, dbe, tmpPath); err != nil {
			e.finishImportFailed(id, "pre-restore dump failed: "+err.Error(), true)
			return
		}
		if err := e.Local.Put(ctx, safetyName, tmpPath); err != nil {
			e.finishImportFailed(id, "store pre-restore dump: "+err.Error(), true)
			return
		}
		e.recordSafetyDump(dbe, safetyName)
	}

	if err := runRestore(ctx, dbe, filepath.Join(e.Local.Root, filename)); err != nil {
		e.finishImportFailed(id, err.Error(), true)
		return
	}
	info, err := os.Stat(filepath.Join(e.Local.Root, filename))
	if err != nil {
		e.finishImportFailed(id, "stat uploaded dump: "+err.Error(), false)
		return
	}
	finished := db.Now()
	if err := e.DB.UpdateBackup(db.Backup{
		ID: id, Status: db.StatusCompleted, FinishedAt: &finished, SizeBytes: info.Size(),
		StoredLocal: true, RestoredAt: &finished,
	}); err != nil {
		slog.Error("import: update row", "id", id, "err", err)
	}
	slog.Info("import finished", "database", dbe.Name, "file", filename, "size", info.Size())
}

// finishImportFailed marks the import row failed. storedLocal reports
// whether the uploaded file remains on disk: it stays downloadable from
// Executions for inspection, except when the upload itself never landed.
func (e *Engine) finishImportFailed(id int64, msg string, storedLocal bool) error {
	finished := db.Now()
	if err := e.DB.UpdateBackup(db.Backup{
		ID: id, Status: db.StatusFailed, FinishedAt: &finished, Error: msg, StoredLocal: storedLocal,
	}); err != nil {
		slog.Error("import: update row", "id", id, "err", err)
	}
	return errors.New(msg)
}

// recordSafetyDump logs the completed pre-restore dump as its own execution
// row (local-only, like the import itself). Row-keeping failures are logged,
// not fatal: the dump file is already stored.
func (e *Engine) recordSafetyDump(dbe db.Database, safetyName string) {
	sid, err := e.DB.CreateBackup(0, db.StatusRunning, TriggerPreRestore, db.Now(), safetyName)
	if err != nil {
		slog.Error("import: record safety dump row", "err", err)
		return
	}
	info, err := os.Stat(filepath.Join(e.Local.Root, safetyName))
	if err != nil {
		slog.Error("import: stat safety dump", "err", err)
		return
	}
	finished := db.Now()
	if err := e.DB.UpdateBackup(db.Backup{
		ID: sid, Status: db.StatusCompleted, FinishedAt: &finished,
		SizeBytes: info.Size(), StoredLocal: true,
	}); err != nil {
		slog.Error("import: update safety dump row", "id", sid, "err", err)
	}
}
