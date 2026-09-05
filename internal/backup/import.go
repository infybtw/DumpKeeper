package backup

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// ImportOptions tunes an uploaded-dump import.
type ImportOptions struct {
	// BackupFirst takes a safety dump of the target before restoring.
	BackupFirst bool
	// CreateDB creates the target database (from template0) when it does
	// not exist on the server yet. Then there is nothing to back up, so
	// BackupFirst only applies to an existing database.
	CreateDB bool
	// KeepACLs replays ownership and privilege statements as-is instead of
	// dropping them; every role they mention must exist on the target.
	KeepACLs bool
	// ClearDB restores into a clean slate: an existing target database is
	// dropped (force-closing its connections) and recreated empty from
	// template0; a missing one is simply created. The safety dump, if
	// requested, runs before the drop.
	ClearDB bool
}

// StartImport records an import row, spools the uploaded dump into the local
// backup directory, and starts the restore — optionally after a safety dump
// of the target database — in a goroutine. It returns immediately; live
// status shows up in Executions. Imports are serialized engine-wide: a
// second concurrent import is rejected instead of racing the restore.
func (e *Engine) StartImport(dbe db.Database, src io.Reader, opts ImportOptions) (int64, string, error) {
	if !e.importMu.TryLock() {
		return 0, "", errors.New("another import or restore is already running")
	}
	filename := sanitizeFilename(dbe.DBName) + "-uploaded-" + time.Now().UTC().Format("20060102T150405Z") + ".sql"
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
	// the restore mid-write.
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer e.importMu.Unlock()
		e.importRun(id, dbe, filename, opts)
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
// skipped safety dump would be worse than not restoring. When ClearDB is
// set the existing database is dropped and recreated empty (safety dump
// runs first); when the database does not exist and CreateDB is set, it is
// created (from template0) before the restore.
func (e *Engine) importRun(id int64, dbe db.Database, filename string, opts ImportOptions) {
	ctx := context.Background()
	p := e.startProgress(id, "preparing")
	defer e.progress.Delete(id) // once gone, the row badge shows the outcome

	// Probe only when a flag needs the answer: without them a missing
	// database fails the restore with the server's own FATAL, which is the
	// message the user expects.
	exists := true
	if opts.CreateDB || opts.ClearDB {
		var err error
		exists, err = databaseExists(ctx, dbe)
		if err != nil {
			e.finishImportFailed(id, "check database: "+err.Error(), true)
			return
		}
	}
	if opts.BackupFirst && !exists {
		slog.Info("import: skipping pre-restore dump, database does not exist", "database", dbe.DBName)
	}

	if opts.BackupFirst && exists {
		p.setPhase("backing up current database")
		safetyName := sanitizeFilename(dbe.DBName) + "-pre-restore-" + time.Now().UTC().Format("20060102T150405Z") + ".sql"
		tmp, err := os.CreateTemp("", "dk-*.sql")
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

	cleared := false
	if opts.ClearDB && exists {
		p.setPhase("clearing database")
		if err := dropDatabase(ctx, dbe); err != nil {
			e.finishImportFailed(id, "clear database: "+err.Error(), true)
			return
		}
		cleared = true
		slog.Info("import: cleared database", "database", dbe.DBName)
	}

	// Restore needs the database to exist: create it when it is missing
	// (and creation was requested) or right after clearing it.
	if cleared || !exists {
		p.setPhase("creating database")
		if err := createDatabase(ctx, dbe); err != nil {
			e.finishImportFailed(id, "create database: "+err.Error(), true)
			return
		}
		slog.Info("import: created database", "database", dbe.DBName)
	}

	// Uploaded dumps routinely reference roles from their source server
	// only; unless the user asked to keep access rights, replay a filtered
	// copy, as pg_restore --no-owner --no-privileges would for a
	// custom-format dump. Legacy .dump files keep going through pg_restore,
	// which has the flags natively.
	restorePath := filepath.Join(e.Local.Root, filename)
	if filepath.Ext(filename) == ".sql" && !opts.KeepACLs {
		replay, cleanup, err := restoreCopy(restorePath)
		if err != nil {
			e.finishImportFailed(id, "prepare upload for restore: "+err.Error(), true)
			return
		}
		defer cleanup()
		restorePath = replay
	}
	if err := runRestore(ctx, dbe, restorePath, opts.KeepACLs, p); err != nil {
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

// restoreCopy writes a replayable copy of the uploaded .sql dump and returns
// its path plus a cleanup func. Uploaded dumps routinely carry ownership and
// privilege statements for roles that exist on their source server only
// (`ALTER ... OWNER TO`, GRANT/REVOKE, ALTER DEFAULT PRIVILEGES); dropping
// them is the plain-text equivalent of pg_restore --no-owner --no-privileges
// and matches how this tool's own dumps are produced. COPY data blocks pass
// through untouched: a data line may legitimately start with "GRANT ". The
// stored upload itself is never modified.
func restoreCopy(srcPath string) (string, func(), error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", nil, err
	}
	defer src.Close()
	dst, err := os.CreateTemp("", "dk-import-*.sql")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.Remove(dst.Name()) }
	fail := func(err error) (string, func(), error) {
		dst.Close()
		cleanup()
		return "", nil, err
	}

	// pg_dump's plain format is line-oriented: one statement per line, and
	// COPY data terminated by a "\." line.
	reader := bufio.NewReader(src)
	writer := bufio.NewWriter(dst)
	inCopy := false
	for {
		line, rerr := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		keep := true
		if inCopy {
			if trimmed == `\.` {
				inCopy = false
			}
		} else {
			switch {
			case strings.HasPrefix(trimmed, "COPY ") && strings.HasSuffix(trimmed, " FROM stdin;"):
				inCopy = true
			case strings.HasPrefix(trimmed, "GRANT "),
				strings.HasPrefix(trimmed, "REVOKE "),
				strings.HasPrefix(trimmed, "ALTER DEFAULT PRIVILEGES"),
				strings.HasPrefix(trimmed, "ALTER ") && strings.Contains(trimmed, " OWNER TO "):
				keep = false
			}
		}
		if keep {
			if _, werr := writer.WriteString(line); werr != nil {
				return fail(werr)
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return fail(rerr)
			}
			break
		}
	}
	if err := writer.Flush(); err != nil {
		return fail(err)
	}
	if err := dst.Close(); err != nil {
		return fail(err)
	}
	return dst.Name(), cleanup, nil
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
