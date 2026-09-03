package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"dumpkeeper/internal/backup"
	"dumpkeeper/internal/db"

	"github.com/robfig/cron/v3"
)

// jobsFragmentData backs both the dashboard page and the htmx poll fragment.
type jobsFragmentData struct {
	Jobs []jobRow
	CSRF string
}

// jobRow is the display shape of one job row.
type jobRow struct {
	ID          int64
	Name        string
	Target      string
	Schedule    string
	DestLocal   bool
	DestS3      bool
	KeepLast    int64
	LastStatus  string
	LastStarted string
	LastError   string
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	rows, err := s.jobRows()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.page(w, r, "dashboard.html", "Jobs", http.StatusOK, jobsFragmentData{Jobs: rows, CSRF: sessionFrom(r).CSRF})
}

func (s *Server) jobsFragment(w http.ResponseWriter, r *http.Request) {
	rows, err := s.jobRows()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderFragment(w, "fragment_jobs.html", "jobs-rows", jobsFragmentData{Jobs: rows, CSRF: sessionFrom(r).CSRF})
}

// jobRows joins jobs with their most recent backup for display.
func (s *Server) jobRows() ([]jobRow, error) {
	jobs, err := s.db.ListJobs()
	if err != nil {
		return nil, err
	}
	latest, err := s.db.LatestBackups()
	if err != nil {
		return nil, err
	}
	rows := make([]jobRow, 0, len(jobs))
	for _, j := range jobs {
		row := jobRow{
			ID:        j.ID,
			Name:      j.Name,
			Target:    fmt.Sprintf("%s:%d/%s", j.Host, j.Port, j.DBName),
			Schedule:  j.Schedule,
			DestLocal: j.DestLocal,
			DestS3:    j.DestS3,
			KeepLast:  j.KeepLast,
		}
		if b, ok := latest[j.ID]; ok {
			row.LastStatus = b.Status
			row.LastError = b.Error
			if t, err := db.ParseTime(b.StartedAt); err == nil {
				row.LastStarted = t.Local().Format("2006-01-02 15:04:05")
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// jobForm is the display shape of the job create/edit form.
type jobForm struct {
	Action    string
	Error     string
	ID        int64
	Name      string
	Host      string
	Port      string
	Username  string
	Password  string
	DBName    string
	SSLMode   string
	Schedule  string
	KeepLast  string
	DestLocal bool
	DestS3    bool
}

// IsNew distinguishes create from edit in the template.
func (f jobForm) IsNew() bool { return f.ID == 0 }

func defaultJobForm(action string) jobForm {
	return jobForm{Action: action, Port: "5432", SSLMode: "prefer", KeepLast: "7", DestLocal: true}
}

func jobAction(id int64) string {
	if id == 0 {
		return "/jobs/new"
	}
	return fmt.Sprintf("/jobs/%d/edit", id)
}

func (s *Server) jobNewForm(w http.ResponseWriter, r *http.Request) {
	f := defaultJobForm("/jobs/new")
	s.page(w, r, "job_form.html", "New job", http.StatusOK, f)
}

func (s *Server) jobCreate(w http.ResponseWriter, r *http.Request) {
	job, f := s.parseJobForm(r, 0)
	if f.Error != "" {
		s.page(w, r, "job_form.html", "New job", http.StatusBadRequest, f)
		return
	}
	created, err := s.db.CreateJob(job)
	if err != nil {
		f.Error = "Could not save job: " + err.Error()
		s.page(w, r, "job_form.html", "New job", http.StatusBadRequest, f)
		return
	}
	s.sched.Reschedule(created)
	redirectTo(w, r, "/", "Job "+created.Name+" created.", "")
}

func (s *Server) jobEditForm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	job, err := s.db.GetJob(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f := jobForm{
		Action: jobAction(job.ID), ID: job.ID,
		Name: job.Name, Host: job.Host, Port: strconv.Itoa(job.Port),
		Username: job.Username, Password: job.Password, DBName: job.DBName,
		SSLMode: job.SSLMode, Schedule: job.Schedule,
		KeepLast:  strconv.FormatInt(job.KeepLast, 10),
		DestLocal: job.DestLocal, DestS3: job.DestS3,
	}
	s.page(w, r, "job_form.html", "Edit job", http.StatusOK, f)
}

func (s *Server) jobUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	job, f := s.parseJobForm(r, id)
	if f.Error != "" {
		s.page(w, r, "job_form.html", "Edit job", http.StatusBadRequest, f)
		return
	}
	if err := s.db.UpdateJob(job); err != nil {
		f.Error = "Could not save job: " + err.Error()
		s.page(w, r, "job_form.html", "Edit job", http.StatusBadRequest, f)
		return
	}
	s.sched.Reschedule(job)
	redirectTo(w, r, "/", "Job "+job.Name+" updated.", "")
}

func (s *Server) jobDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	job, err := s.db.GetJob(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.sched.Remove(job.ID)
	// Best-effort cleanup of the job's stored files; rows cascade next.
	backups, err := s.db.ListBackups(job.ID, false)
	if err != nil {
		slog.Warn("delete job: list backups", "job", job.Name, "err", err)
	}
	for _, b := range backups {
		if b.StoredLocal {
			if err := s.engine.Local.Delete(r.Context(), b.Filename); err != nil {
				slog.Warn("delete job: remove local file", "file", b.Filename, "err", err)
			}
		}
		if b.StoredS3 {
			if err := s.engine.S3.Delete(r.Context(), b.Filename); err != nil {
				slog.Warn("delete job: remove S3 object", "object", b.Filename, "err", err)
			}
		}
	}
	if err := s.db.DeleteJob(job.ID); err != nil {
		redirectTo(w, r, "/", "", "Could not delete job: "+err.Error())
		return
	}
	redirectTo(w, r, "/", "Job "+job.Name+" deleted.", "")
}

func (s *Server) jobBackup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.db.GetJob(id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.engine.Trigger(id, backup.TriggerManual); err != nil {
		if errors.Is(err, backup.ErrAlreadyRunning) {
			redirectTo(w, r, "/", "", "A backup is already running for this job.")
			return
		}
		redirectTo(w, r, "/", "", err.Error())
		return
	}
	redirectTo(w, r, "/", "Backup started.", "")
}

// parseJobForm reads and validates the job form. It always returns the form
// (with entered values) for re-rendering; f.Error != "" means invalid.
func (s *Server) parseJobForm(r *http.Request, id int64) (db.Job, jobForm) {
	f := jobForm{
		ID: id, Action: jobAction(id),
		Name:      strings.TrimSpace(r.FormValue("name")),
		Host:      strings.TrimSpace(r.FormValue("host")),
		Port:      strings.TrimSpace(r.FormValue("port")),
		Username:  strings.TrimSpace(r.FormValue("username")),
		Password:  r.FormValue("password"),
		DBName:    strings.TrimSpace(r.FormValue("dbname")),
		SSLMode:   r.FormValue("sslmode"),
		Schedule:  strings.TrimSpace(r.FormValue("schedule")),
		KeepLast:  strings.TrimSpace(r.FormValue("keep_last")),
		DestLocal: r.FormValue("dest_local") == "1",
		DestS3:    r.FormValue("dest_s3") == "1",
	}
	switch {
	case f.Name == "":
		f.Error = "Name is required."
	case f.Host == "":
		f.Error = "Host is required."
	case f.DBName == "":
		f.Error = "Database name is required."
	}
	port := 0
	if f.Error == "" {
		if f.Port == "" {
			f.Port = "5432"
		}
		p, err := strconv.Atoi(f.Port)
		if err != nil || p < 1 || p > 65535 {
			f.Error = "Port must be a number between 1 and 65535."
		}
		port = p
	}
	keepLast := int64(0)
	if f.Error == "" {
		if f.KeepLast == "" {
			f.KeepLast = "7"
		}
		k, err := strconv.ParseInt(f.KeepLast, 10, 64)
		if err != nil || k < 0 {
			f.Error = "Keep last must be 0 or greater."
		}
		keepLast = k
	}
	if f.Error == "" && f.Schedule != "" {
		if _, err := cron.ParseStandard(f.Schedule); err != nil {
			f.Error = "Invalid cron schedule: " + err.Error()
		}
	}
	if f.Error == "" && !f.DestLocal && !f.DestS3 {
		f.Error = "Choose at least one destination (local or S3)."
	}
	if f.Error == "" {
		exists, err := s.db.JobNameExists(f.Name, id)
		switch {
		case err != nil:
			f.Error = err.Error()
		case exists:
			f.Error = "A job with this name already exists."
		}
	}
	if f.SSLMode == "" {
		f.SSLMode = "prefer"
	}
	job := db.Job{
		ID: id, Name: f.Name, Host: f.Host,
		Username: f.Username, Password: f.Password, DBName: f.DBName,
		SSLMode: f.SSLMode, Schedule: f.Schedule,
		DestLocal: f.DestLocal, DestS3: f.DestS3,
		Port: port, KeepLast: keepLast,
	}
	return job, f
}
