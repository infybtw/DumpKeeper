package backup

import (
	"context"
	"log/slog"

	"dumpkeeper/internal/db"
)

// Prune enforces keep-last-N retention on a job's completed backups, newest
// first: rows beyond job.KeepLast have their stored files deleted (local
// copy and every S3 destination holding them), then their rows. Individual
// deletion failures are logged and never abort the loop. KeepLast 0 means
// unlimited.
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
		dests, err := e.DB.BackupDestinations(b.ID)
		if err != nil {
			slog.Warn("retention: list destinations", "job", job.Name, "backup", b.ID, "err", err)
			continue
		}
		for _, d := range dests {
			if err := S3Store(d).Delete(ctx, b.Filename); err != nil {
				slog.Warn("retention: delete S3 object", "job", job.Name, "destination", d.Name, "object", b.Filename, "err", err)
			}
		}
		if err := e.DB.DeleteBackup(b.ID); err != nil {
			slog.Warn("retention: delete row", "job", job.Name, "backup", b.ID, "err", err)
		}
	}
}
