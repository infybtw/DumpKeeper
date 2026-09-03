package backup

import (
	"context"
	"log/slog"

	"dumpkeeper/internal/db"
)

// Prune enforces keep-last-N retention on job's completed backups, newest
// first: rows beyond job.KeepLast have their stored files (local and/or S3)
// deleted, then their rows. Individual deletion failures are logged and
// never abort the loop. KeepLast 0 means unlimited.
func (e *Engine) Prune(job db.Job) {
	if job.KeepLast <= 0 {
		return
	}
	ctx := context.Background()
	backups, err := e.DB.ListBackups(job.ID, true)
	if err != nil {
		slog.Error("retention: list backups", "job", job.Name, "err", err)
		return
	}
	for i, b := range backups {
		if i < int(job.KeepLast) {
			continue
		}
		if b.StoredLocal {
			if err := e.Local.Delete(ctx, b.Filename); err != nil {
				slog.Warn("retention: delete local file", "job", job.Name, "file", b.Filename, "err", err)
			}
		}
		if b.StoredS3 {
			if err := e.S3.Delete(ctx, b.Filename); err != nil {
				slog.Warn("retention: delete S3 object", "job", job.Name, "object", b.Filename, "err", err)
			}
		}
		if err := e.DB.DeleteBackup(b.ID); err != nil {
			slog.Warn("retention: delete row", "job", job.Name, "backup", b.ID, "err", err)
		}
	}
}
