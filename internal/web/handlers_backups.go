package web

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"dumpkeeper/internal/db"
)

// backupRow is the display shape of one backups-table row.
type backupRow struct {
	ID          int64
	JobID       int64
	JobName     string
	Status      string
	Trigger     string
	StartedAt   string
	FinishedAt  string
	Size        string
	StoredLocal bool
	StoredS3    bool
	HasFile     bool
	Error       string
}

func (s *Server) backupsList(w http.ResponseWriter, r *http.Request) {
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
	names := make(map[int64]string, len(jobs))
	for _, j := range jobs {
		names[j.ID] = j.Name
	}
	list, err := s.db.ListBackups(jobFilter, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]backupRow, 0, len(list))
	for _, b := range list {
		rows = append(rows, toBackupRow(b, names[b.JobID]))
	}
	s.page(w, r, "backups.html", "Backups", http.StatusOK, struct {
		Jobs     []db.Job
		Selected int64
		Backups  []backupRow
	}{jobs, jobFilter, rows})
}

func (s *Server) backupRestore(w http.ResponseWriter, r *http.Request) {
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
		redirectTo(w, r, "/backups", "", err.Error())
		return
	}
	redirectTo(w, r, "/backups", "Restored "+b.Filename+" into the job's database.", "")
}

func (s *Server) backupDelete(w http.ResponseWriter, r *http.Request) {
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
	if b.StoredLocal {
		if err := s.engine.Local.Delete(r.Context(), b.Filename); err != nil {
			redirectTo(w, r, "/backups", "", "Could not delete local file: "+err.Error())
			return
		}
	}
	if b.StoredS3 {
		if err := s.engine.S3.Delete(r.Context(), b.Filename); err != nil {
			redirectTo(w, r, "/backups", "", "Could not delete S3 object: "+err.Error())
			return
		}
	}
	if err := s.db.DeleteBackup(b.ID); err != nil {
		redirectTo(w, r, "/backups", "", err.Error())
		return
	}
	redirectTo(w, r, "/backups", "Backup deleted.", "")
}

func (s *Server) backupDownload(w http.ResponseWriter, r *http.Request) {
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
	if !b.StoredLocal && !b.StoredS3 {
		http.Error(w, "backup file is not available", http.StatusNotFound)
		return
	}
	var rc io.ReadCloser
	var size int64
	if b.StoredLocal {
		rc, size, err = s.engine.Local.Open(r.Context(), b.Filename)
	} else {
		rc, size, err = s.engine.S3.Open(r.Context(), b.Filename)
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

func toBackupRow(b db.Backup, jobName string) backupRow {
	row := backupRow{
		ID: b.ID, JobID: b.JobID, JobName: jobName,
		Status: b.Status, Trigger: b.Trigger,
		StoredLocal: b.StoredLocal, StoredS3: b.StoredS3,
		Error:   b.Error,
		HasFile: b.Status == db.StatusCompleted && (b.StoredLocal || b.StoredS3),
		Size:    humanSize(b.SizeBytes),
	}
	if t, err := db.ParseTime(b.StartedAt); err == nil {
		row.StartedAt = t.Local().Format("2006-01-02 15:04:05")
	}
	if b.FinishedAt != nil {
		if t, err := db.ParseTime(*b.FinishedAt); err == nil {
			row.FinishedAt = t.Local().Format("2006-01-02 15:04:05")
		}
	}
	return row
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
