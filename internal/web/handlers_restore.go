package web

import (
	"net/http"

	"dumpkeeper/internal/backup"
	"dumpkeeper/internal/db"
)

// restorePage shows the manual restore form: pick a database profile and
// upload a SQL dump, optionally taking a safety dump and/or creating a
// missing database first.
func (s *Server) restorePage(w http.ResponseWriter, r *http.Request) {
	dbs, err := s.db.ListDatabases()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.page(w, r, "restore.html", "Restore", http.StatusOK, struct {
		Databases []db.Database
	}{dbs})
}

// restoreSubmit spools the uploaded dump and hands it to the engine. The
// restore runs asynchronously; progress shows up in Executions. requireAuth
// has already validated CSRF via PostFormValue, which also triggered
// multipart parsing, so MultipartForm is populated here.
func (s *Server) restoreSubmit(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PostFormValue("database_id"))
	if err != nil {
		redirectTo(w, r, "/restore", "", "select a database profile")
		return
	}
	dbe, err := s.db.GetDatabase(id)
	if err != nil {
		redirectTo(w, r, "/restore", "", "select a database profile")
		return
	}
	backupFirst := r.PostFormValue("backup_first") == "1"
	createDB := r.PostFormValue("create_db") == "1"
	clearDB := r.PostFormValue("clear_db") == "1"
	keepACLs := r.PostFormValue("keep_acls") == "1"
	fhs := r.MultipartForm.File["file"]
	if len(fhs) != 1 {
		redirectTo(w, r, "/restore", "", "attach exactly one .sql file")
		return
	}
	if fhs[0].Size == 0 {
		redirectTo(w, r, "/restore", "", "uploaded file is empty")
		return
	}
	src, err := fhs[0].Open()
	if err != nil {
		redirectTo(w, r, "/restore", "", "open upload: "+err.Error())
		return
	}
	defer src.Close()
	_, filename, err := s.engine.StartImport(dbe, src, backup.ImportOptions{BackupFirst: backupFirst, CreateDB: createDB, KeepACLs: keepACLs, ClearDB: clearDB})
	if err != nil {
		redirectTo(w, r, "/restore", "", err.Error())
		return
	}
	msg := "Import started: " + filename + " into " + dbe.DBName
	if backupFirst {
		msg += ", safety dump first"
	}
	redirectTo(w, r, "/executions", msg+".", "")
}
