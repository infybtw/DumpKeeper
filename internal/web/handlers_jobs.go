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
	ID               int64
	Name             string
	DatabaseName     string
	Target           string
	Schedule         string
	DestLocal        bool
	DestinationNames []string
	KeepLast         int64
	LastStatus       string
	LastStarted      string
	LastError        string
}

// jobsPage renders the backup jobs list (linked from the sidebar; the main
// page is the dashboard).
func (s *Server) jobsPage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.jobRows()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.page(w, r, "jobs.html", "Jobs", http.StatusOK, jobsFragmentData{Jobs: rows, CSRF: sessionFrom(r).CSRF})
}

func (s *Server) jobsFragment(w http.ResponseWriter, r *http.Request) {
	rows, err := s.jobRows()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderFragment(w, "fragment_jobs.html", "jobs-rows", jobsFragmentData{Jobs: rows, CSRF: sessionFrom(r).CSRF})
}

// jobRows joins jobs with their database and most recent execution.
func (s *Server) jobRows() ([]jobRow, error) {
	jobs, err := s.db.ListJobs()
	if err != nil {
		return nil, err
	}
	latest, err := s.db.LatestBackups()
	if err != nil {
		return nil, err
	}
	databases := map[int64]db.Database{}
	if dbs, err := s.db.ListDatabases(); err == nil {
		for _, d := range dbs {
			databases[d.ID] = d
		}
	}
	rows := make([]jobRow, 0, len(jobs))
	for _, j := range jobs {
		row := jobRow{
			ID:        j.ID,
			Name:      j.Name,
			Schedule:  j.Schedule,
			DestLocal: j.DestLocal,
			KeepLast:  j.KeepLast,
		}
		if d, ok := databases[j.DatabaseID]; ok {
			row.DatabaseName = d.Name
			row.Target = fmt.Sprintf("%s:%d/%s", d.Host, d.Port, d.DBName)
		}
		for _, id := range j.DestinationIDs {
			if d, err := s.db.GetDestination(id); err == nil {
				row.DestinationNames = append(row.DestinationNames, d.Name)
			}
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
	Action   string
	Error    string
	ID       int64
	Name     string
	Schedule string
	KeepLast string

	DatabaseID     int64
	DestLocal      bool
	DestinationIDs []int64

	Databases    []db.Database
	Destinations []db.Destination
}

// IsNew distinguishes create from edit in the template.
func (f jobForm) IsNew() bool { return f.ID == 0 }

func (s *Server) defaultJobForm(action string) jobForm {
	f := jobForm{Action: action, KeepLast: "7", DestLocal: true}
	f.Databases, _ = s.db.ListDatabases()
	f.Destinations, _ = s.db.ListDestinations()
	return f
}

func jobAction(id int64) string {
	if id == 0 {
		return "/jobs/new"
	}
	return fmt.Sprintf("/jobs/%d/edit", id)
}

func (s *Server) jobNewForm(w http.ResponseWriter, r *http.Request) {
	f := s.defaultJobForm("/jobs/new")
	if len(f.Databases) == 0 {
		f.Error = "Create a database first."
	}
	if isHtmx(r) {
		s.jobModal(w, r, f)
		return
	}
	s.page(w, r, "job_form.html", "New job", http.StatusOK, f)
}

// jobModal writes the job modal fragment (create and edit). Status 200:
// htmx 2.x does not swap 4xx responses.
func (s *Server) jobModal(w http.ResponseWriter, r *http.Request, f jobForm) {
	s.renderModal(w, http.StatusOK, "fragment_job_form.html", "job-modal", sessionFrom(r).CSRF, f)
}

func (s *Server) jobCreate(w http.ResponseWriter, r *http.Request) {
	job, f := s.parseJobForm(r, 0)
	if f.Error != "" {
		if isHtmx(r) {
			s.jobModal(w, r, f)
			return
		}
		s.page(w, r, "job_form.html", "New job", http.StatusBadRequest, f)
		return
	}
	created, err := s.db.CreateJob(job)
	if err != nil {
		f.Error = "Could not save job: " + err.Error()
		if isHtmx(r) {
			s.jobModal(w, r, f)
			return
		}
		s.page(w, r, "job_form.html", "New job", http.StatusBadRequest, f)
		return
	}
	s.sched.Reschedule(created)
	if isHtmx(r) {
		s.htmxRedirect(w, "/", "Job "+created.Name+" created.", "")
		return
	}
	s.redirectTo(w, r, "/", "Job "+created.Name+" created.", "")
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
	f := s.defaultJobForm(jobAction(job.ID))
	f.ID = job.ID
	f.Name = job.Name
	f.Schedule = job.Schedule
	f.KeepLast = strconv.FormatInt(job.KeepLast, 10)
	f.DatabaseID = job.DatabaseID
	f.DestLocal = job.DestLocal
	f.DestinationIDs = job.DestinationIDs
	if isHtmx(r) {
		s.jobModal(w, r, f)
		return
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
		if isHtmx(r) {
			s.jobModal(w, r, f)
			return
		}
		s.page(w, r, "job_form.html", "Edit job", http.StatusBadRequest, f)
		return
	}
	if err := s.db.UpdateJob(job); err != nil {
		f.Error = "Could not save job: " + err.Error()
		if isHtmx(r) {
			s.jobModal(w, r, f)
			return
		}
		s.page(w, r, "job_form.html", "Edit job", http.StatusBadRequest, f)
		return
	}
	s.sched.Reschedule(job)
	if isHtmx(r) {
		s.htmxRedirect(w, "/", "Job "+job.Name+" updated.", "")
		return
	}
	s.redirectTo(w, r, "/", "Job "+job.Name+" updated.", "")
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
		s.deleteBackupFiles(r, b)
	}
	if err := s.db.DeleteJob(job.ID); err != nil {
		s.redirectTo(w, r, "/", "", "Could not delete job: "+err.Error())
		return
	}
	s.redirectTo(w, r, "/", "Job "+job.Name+" deleted.", "")
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
			s.redirectTo(w, r, "/", "", "A backup is already running for this job.")
			return
		}
		s.redirectTo(w, r, "/", "", err.Error())
		return
	}
	s.redirectTo(w, r, "/", "Backup started.", "")
}

// parseJobForm reads and validates the job form. It always returns the form
// (with entered values) for re-rendering; f.Error != "" means invalid.
func (s *Server) parseJobForm(r *http.Request, id int64) (db.Job, jobForm) {
	f := jobForm{
		ID:        id,
		Action:    jobAction(id),
		Name:      strings.TrimSpace(r.FormValue("name")),
		Schedule:  strings.TrimSpace(r.FormValue("schedule")),
		KeepLast:  strings.TrimSpace(r.FormValue("keep_last")),
		DestLocal: r.FormValue("dest_local") == "1",
	}
	f.Databases, _ = s.db.ListDatabases()
	f.Destinations, _ = s.db.ListDestinations()
	for _, v := range r.Form["dest"] {
		if did, err := parseID(v); err == nil {
			f.DestinationIDs = append(f.DestinationIDs, did)
		}
	}
	if did, err := parseID(r.FormValue("database_id")); err == nil {
		f.DatabaseID = did
	}

	switch {
	case f.Name == "":
		f.Error = "Name is required."
	case f.DatabaseID == 0:
		f.Error = "Choose a database."
	default:
		if _, err := s.db.GetDatabase(f.DatabaseID); err != nil {
			f.Error = "Unknown database."
		}
	}
	if f.Error == "" && !f.DestLocal && len(f.DestinationIDs) == 0 {
		f.Error = "Choose at least one destination (local or S3)."
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
	if f.Error == "" {
		exists, err := s.db.JobNameExists(f.Name, id)
		switch {
		case err != nil:
			f.Error = err.Error()
		case exists:
			f.Error = "A job with this name already exists."
		}
	}
	job := db.Job{
		ID: id, Name: f.Name, DatabaseID: f.DatabaseID,
		Schedule: f.Schedule, DestLocal: f.DestLocal,
		KeepLast: keepLast, DestinationIDs: f.DestinationIDs,
	}
	return job, f
}
