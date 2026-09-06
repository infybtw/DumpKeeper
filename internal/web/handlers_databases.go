package web

import (
	"net/http"
	"strconv"
	"strings"

	"dumpkeeper/internal/db"
)

// databaseRow is a database plus its usage count for the list page.
type databaseRow struct {
	db.Database
	JobCount int
}

// databaseForm is the display shape of the database create/edit form.
type databaseForm struct {
	Action   string
	Error    string
	ID       int64
	Name     string
	Host     string
	Port     string
	Username string
	Password string
	DBName   string
	SSLMode  string
}

// IsNew distinguishes create from edit in the template.
func (f databaseForm) IsNew() bool { return f.ID == 0 }

func defaultDatabaseForm(action string) databaseForm {
	return databaseForm{Action: action, Port: "5432", SSLMode: "prefer"}
}

func databaseAction(id int64) string {
	if id == 0 {
		return "/databases/new"
	}
	return "/databases/" + strconv.FormatInt(id, 10) + "/edit"
}

func (s *Server) databasesList(w http.ResponseWriter, r *http.Request) {
	dbs, err := s.db.ListDatabases()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]databaseRow, 0, len(dbs))
	for _, d := range dbs {
		row := databaseRow{Database: d}
		row.JobCount, _ = s.db.DatabaseJobCount(d.ID)
		rows = append(rows, row)
	}
	s.page(w, r, "databases.html", "Databases", http.StatusOK, rows)
}

func (s *Server) databaseNewForm(w http.ResponseWriter, r *http.Request) {
	f := defaultDatabaseForm("/databases/new")
	if isHtmx(r) {
		s.databaseModal(w, r, f)
		return
	}
	s.page(w, r, "database_form.html", "New database", http.StatusOK, f)
}

// databaseModal writes the database modal fragment (create and edit). Status
// 200: htmx 2.x does not swap 4xx responses.
func (s *Server) databaseModal(w http.ResponseWriter, r *http.Request, f databaseForm) {
	s.renderModal(w, http.StatusOK, "fragment_database_form.html", "database-modal", sessionFrom(r).CSRF, f)
}

func (s *Server) databaseCreate(w http.ResponseWriter, r *http.Request) {
	dbe, f := s.parseDatabaseForm(r, 0)
	if f.Error != "" {
		if isHtmx(r) {
			s.databaseModal(w, r, f)
			return
		}
		s.page(w, r, "database_form.html", "New database", http.StatusBadRequest, f)
		return
	}
	created, err := s.db.CreateDatabase(dbe)
	if err != nil {
		f.Error = "Could not save database: " + err.Error()
		if isHtmx(r) {
			s.databaseModal(w, r, f)
			return
		}
		s.page(w, r, "database_form.html", "New database", http.StatusBadRequest, f)
		return
	}
	if isHtmx(r) {
		s.htmxRedirect(w, "/databases", "Database "+created.Name+" created.", "")
		return
	}
	s.redirectTo(w, r, "/databases", "Database "+created.Name+" created.", "")
}

func (s *Server) databaseEditForm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	dbe, err := s.db.GetDatabase(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f := databaseForm{
		Action: databaseAction(dbe.ID), ID: dbe.ID,
		Name: dbe.Name, Host: dbe.Host, Port: strconv.Itoa(dbe.Port),
		Username: dbe.Username, Password: dbe.Password, DBName: dbe.DBName,
		SSLMode: dbe.SSLMode,
	}
	if isHtmx(r) {
		s.databaseModal(w, r, f)
		return
	}
	s.page(w, r, "database_form.html", "Edit database", http.StatusOK, f)
}

func (s *Server) databaseUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	dbe, f := s.parseDatabaseForm(r, id)
	if f.Error != "" {
		if isHtmx(r) {
			s.databaseModal(w, r, f)
			return
		}
		s.page(w, r, "database_form.html", "Edit database", http.StatusBadRequest, f)
		return
	}
	if err := s.db.UpdateDatabase(dbe); err != nil {
		f.Error = "Could not save database: " + err.Error()
		if isHtmx(r) {
			s.databaseModal(w, r, f)
			return
		}
		s.page(w, r, "database_form.html", "Edit database", http.StatusBadRequest, f)
		return
	}
	if isHtmx(r) {
		s.htmxRedirect(w, "/databases", "Database "+dbe.Name+" updated.", "")
		return
	}
	s.redirectTo(w, r, "/databases", "Database "+dbe.Name+" updated.", "")
}

// databasePing checks and records availability just like the background monitor.
func (s *Server) databasePing(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	dbe, err := s.db.GetDatabase(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	result, err := s.mon.Check(r.Context(), dbe)
	if err != nil {
		s.redirectTo(w, r, "/databases", "", "Could not record database check: "+err.Error())
		return
	}
	if !result.OK {
		s.redirectTo(w, r, "/databases", "", "Could not reach database "+dbe.Name+": "+result.Error)
		return
	}
	s.redirectTo(w, r, "/databases", "Database "+dbe.Name+" is reachable.", "")
}

func (s *Server) databaseDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	dbe, err := s.db.GetDatabase(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if n, err := s.db.DatabaseJobCount(dbe.ID); err == nil && n > 0 {
		s.redirectTo(w, r, "/databases", "",
			"Database "+dbe.Name+" is used by "+strconv.Itoa(n)+" job(s); remove those jobs first.")
		return
	}
	if err := s.db.DeleteDatabase(dbe.ID); err != nil {
		s.redirectTo(w, r, "/databases", "", "Could not delete database: "+err.Error())
		return
	}
	s.redirectTo(w, r, "/databases", "Database "+dbe.Name+" deleted.", "")
}

// parseDatabaseForm reads and validates the database form; f.Error != ""
// means invalid and the form is pre-filled for re-rendering.
func (s *Server) parseDatabaseForm(r *http.Request, id int64) (db.Database, databaseForm) {
	f := databaseForm{
		ID: id, Action: databaseAction(id),
		Name:     strings.TrimSpace(r.FormValue("name")),
		Host:     strings.TrimSpace(r.FormValue("host")),
		Port:     strings.TrimSpace(r.FormValue("port")),
		Username: strings.TrimSpace(r.FormValue("username")),
		Password: r.FormValue("password"),
		DBName:   strings.TrimSpace(r.FormValue("dbname")),
		SSLMode:  r.FormValue("sslmode"),
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
	if f.Error == "" && f.SSLMode == "" {
		f.SSLMode = "prefer"
	}
	if f.Error == "" {
		exists, err := s.db.DatabaseNameExists(f.Name, id)
		switch {
		case err != nil:
			f.Error = err.Error()
		case exists:
			f.Error = "A database with this name already exists."
		}
	}
	return db.Database{
		ID: id, Name: f.Name, Host: f.Host,
		Username: f.Username, Password: f.Password, DBName: f.DBName,
		SSLMode: f.SSLMode, Port: port,
	}, f
}
