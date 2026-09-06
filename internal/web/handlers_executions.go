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
	CanRestore bool // job-bound rows only: imports have no job to restore into
	Error      string

	CSRF     string
	Progress *backup.RestoreProgress // live/terminal restore state, nil = none
	Percent  int                     // replayed bytes, when Total is known
	Live     bool                    // row polls itself while something runs
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
		rows = append(rows, s.buildRow(r, b, jobNames, databaseIDs, databases, storedOn))
	}
	s.page(w, r, "executions.html", "Executions", http.StatusOK, struct {
		Jobs     []db.Job
		Selected int64
		Backups  []executionRow
	}{jobs, jobFilter, rows})
}

// buildRow assembles the display row for b and attaches live restore state.
// The row polls itself (htmx) while a backup or restore is in flight.
func (s *Server) buildRow(r *http.Request, b db.Backup, jobNames map[int64]string,
	databaseIDs map[int64]int64, databases map[int64]string, storedOn map[int64][]string,
) executionRow {
	row := toExecutionRow(b, jobNames[b.JobID], databases[databaseIDs[b.JobID]], storedOn[b.ID])
	row.CSRF = sessionFrom(r).CSRF
	if p, ok := s.engine.Progress(b.ID); ok {
		row.Progress = &p
		if p.Total > 0 {
			row.Percent = int(float64(p.Replayed) / float64(p.Total) * 100)
		}
	}
	row.Live = b.Status == db.StatusRunning || (row.Progress != nil && !row.Progress.Finished)
	return row
}

// executionRestore starts the restore in the background and returns right
// away; the executions row shows live progress and the outcome.
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
	if b.Status != db.StatusCompleted {
		s.redirectTo(w, r, "/executions", "", "only completed executions can be restored")
		return
	}
	if err := s.engine.RestoreAsync(b); err != nil {
		s.redirectTo(w, r, "/executions", "", err.Error())
		return
	}
	s.redirectTo(w, r, "/executions", "Restore started: "+b.Filename+".", "")
}

// executionRowPoll re-renders one executions table row for htmx polling.
// While the row is live the response keeps the poll attribute; once nothing
// runs the attribute is dropped and the polling stops by itself.
func (s *Server) executionRowPoll(w http.ResponseWriter, r *http.Request) {
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
	jobNames := map[int64]string{}
	databaseIDs := map[int64]int64{}
	databases := map[int64]string{}
	if j, err := s.db.GetJob(b.JobID); err == nil {
		jobNames[j.ID] = j.Name
		databaseIDs[j.ID] = j.DatabaseID
		if d, err := s.db.GetDatabase(j.DatabaseID); err == nil {
			databases[d.ID] = d.Name
		}
	}
	storedOn := map[int64][]string{}
	if dests, err := s.db.BackupDestinations(b.ID); err == nil {
		for _, d := range dests {
			storedOn[b.ID] = append(storedOn[b.ID], d.Name)
		}
	}
	s.renderFragment(w, "executions.html", "execution_row", s.buildRow(r, b, jobNames, databaseIDs, databases, storedOn))
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
	s.engine.ClearProgress(b.ID)
	s.deleteBackupFiles(r, b)
	if err := s.db.DeleteBackup(b.ID); err != nil {
		s.redirectTo(w, r, "/executions", "", err.Error())
		return
	}
	s.redirectTo(w, r, "/executions", "Execution deleted (files removed).", "")
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
		CanRestore: b.JobID != 0,
		Error:      b.Error,
		Size:       humanSize(b.SizeBytes),
		HasFile:    b.StoredLocal || len(destNames) > 0,
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

// humanDuration reports elapsed time in milliseconds, rounded to two decimal
// places for a compact, consistent table value.
func humanDuration(d time.Duration) string {
	return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
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
