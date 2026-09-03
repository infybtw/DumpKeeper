package web

import (
	"dumpkeeper/internal/backup"
	"dumpkeeper/internal/db"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// executionRow is the display shape of one executions-table row.
type executionRow struct {
	ID         int64
	JobID      int64
	JobName    string
	Database   string
	Status     string
	Trigger    string
	StartedAt  string
	FinishedAt string
	Duration   string
	Size       string
	StoredOn   []string // "local" + destination names
	HasFile    bool
	Error      string
}

func (s *Server) executionsList(w http.ResponseWriter, r *http.Request) {
	jobFilter := int64(0)
	if v := r.URL.Query().Get("job"); v != "" {
		if id, err := parseID(v); err == nil {
			jobFilter = id
		}
	}
	jobs, err := s.db.ListJobs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jobNames := make(map[int64]string, len(jobs))
	databaseIDs := make(map[int64]int64, len(jobs))
	for _, j := range jobs {
		jobNames[j.ID] = j.Name
		databaseIDs[j.ID] = j.DatabaseID
	}
	databases := map[int64]string{}
	if dbs, err := s.db.ListDatabases(); err == nil {
		for _, d := range dbs {
			databases[d.ID] = d.Name
		}
	}
	storedOn := make(map[int64][]string)
	if links, err := s.db.ListBackupLinks(); err == nil {
		for _, l := range links {
			storedOn[l.BackupID] = append(storedOn[l.BackupID], l.Destination.Name)
		}
	}
	list, err := s.db.ListBackups(jobFilter, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]executionRow, 0, len(list))
	for _, b := range list {
		rows = append(rows, toExecutionRow(b, jobNames[b.JobID], databases[databaseIDs[b.JobID]], storedOn[b.ID]))
	}
	s.page(w, r, "executions.html", "Executions", http.StatusOK, struct {
		Jobs     []db.Job
		Selected int64
		Backups  []executionRow
	}{jobs, jobFilter, rows})
}

func (s *Server) executionRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := s.db.GetBackup(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.engine.Restore(r.Context(), b); err != nil {
		redirectTo(w, r, "/executions", "", err.Error())
		return
	}
	redirectTo(w, r, "/executions", "Restored "+b.Filename+" into the job's database.", "")
}

func (s *Server) executionDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := s.db.GetBackup(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.deleteBackupFiles(r, b)
	if err := s.db.DeleteBackup(b.ID); err != nil {
		redirectTo(w, r, "/executions", "", err.Error())
		return
	}
	redirectTo(w, r, "/executions", "Execution deleted (files removed).", "")
}

func (s *Server) executionDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := s.db.GetBackup(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var rc io.ReadCloser
	var size int64
	if b.StoredLocal {
		rc, size, err = s.engine.Local.Open(r.Context(), b.Filename)
	} else {
		var dests []db.Destination
		dests, err = s.db.BackupDestinations(b.ID)
		if err == nil {
			if len(dests) == 0 {
				http.Error(w, "backup file is not available", http.StatusNotFound)
				return
			}
			rc, size, err = backup.S3Store(dests[0]).Open(r.Context(), b.Filename)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", b.Filename))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	io.Copy(w, rc)
}

// deleteBackupFiles removes a backup's local copy and every stored S3
// object; failures are logged, not fatal.
func (s *Server) deleteBackupFiles(r *http.Request, b db.Backup) {
	if b.StoredLocal {
		if err := s.engine.Local.Delete(r.Context(), b.Filename); err != nil {
			slog.Warn("remove local file", "file", b.Filename, "err", err)
		}
	}
	dests, err := s.db.BackupDestinations(b.ID)
	if err != nil {
		slog.Warn("list backup destinations", "backup", b.ID, "err", err)
		return
	}
	for _, d := range dests {
		if err := backup.S3Store(d).Delete(r.Context(), b.Filename); err != nil {
			slog.Warn("remove S3 object", "destination", d.Name, "object", b.Filename, "err", err)
		}
	}
}

func toExecutionRow(b db.Backup, jobName, databaseName string, destNames []string) executionRow {
	row := executionRow{
		ID: b.ID, JobID: b.JobID, JobName: jobName, Database: databaseName,
		Status: b.Status, Trigger: b.Trigger,
		Error:   b.Error,
		Size:    humanSize(b.SizeBytes),
		HasFile: b.Status == db.StatusCompleted && (b.StoredLocal || len(destNames) > 0),
	}
	if b.StoredLocal {
		row.StoredOn = append(row.StoredOn, "local")
	}
	row.StoredOn = append(row.StoredOn, destNames...)
	var started time.Time
	if t, err := db.ParseTime(b.StartedAt); err == nil {
		started = t
		row.StartedAt = t.Local().Format("2006-01-02 15:04:05")
	}
	if b.FinishedAt != nil {
		if t, err := db.ParseTime(*b.FinishedAt); err == nil {
			row.FinishedAt = t.Local().Format("2006-01-02 15:04:05")
			if !started.IsZero() {
				row.Duration = humanDuration(t.Sub(started))
			}
		}
	} else if b.Status == db.StatusRunning && !started.IsZero() {
		row.Duration = humanDuration(time.Since(started)) + " (running)"
	}
	return row
}

// humanDuration preserves the exact elapsed duration recorded in the
// execution timestamps, including sub-second precision.
func humanDuration(d time.Duration) string {
	return d.String()
}

// humanSize renders byte counts as B/KB/MB/GB/TB.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
