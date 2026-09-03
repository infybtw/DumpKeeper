// Package web serves DumpKeeper's server-rendered UI (stdlib mux + htmx).
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"dumpkeeper/internal/backup"
	"dumpkeeper/internal/config"
	"dumpkeeper/internal/db"
	"dumpkeeper/internal/scheduler"
)

//go:embed templates static
var files embed.FS

// Server owns the HTTP mux and all handlers.
type Server struct {
	cfg    config.Config
	db     *db.Store
	engine *backup.Engine
	sched  *scheduler.Scheduler
	mux    *http.ServeMux
}

// route table (session middleware on everything except /login):
//
//	GET  /login                     POST /login                 POST /logout
//	GET  /                          GET  /fragment/jobs
//	GET  /jobs/new                  POST /jobs/new
//	GET  /jobs/{id}/edit            POST /jobs/{id}/edit        POST /jobs/{id}/delete
//	POST /jobs/{id}/backup
//	GET  /backups                   POST /backups/{id}/restore  POST /backups/{id}/delete
//	GET  /backups/{id}/download
//	GET  /settings                  POST /settings
func New(cfg config.Config, store *db.Store, engine *backup.Engine, sched *scheduler.Scheduler) *Server {
	s := &Server{cfg: cfg, db: store, engine: engine, sched: sched, mux: http.NewServeMux()}
	mux := s.mux

	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.loginSubmit)

	mux.HandleFunc("GET /{$}", s.requireAuth(s.dashboard))
	mux.HandleFunc("GET /fragment/jobs", s.requireAuth(s.jobsFragment))
	mux.HandleFunc("GET /jobs/new", s.requireAuth(s.jobNewForm))
	mux.HandleFunc("POST /jobs/new", s.requireAuth(s.jobCreate))
	mux.HandleFunc("GET /jobs/{id}/edit", s.requireAuth(s.jobEditForm))
	mux.HandleFunc("POST /jobs/{id}/edit", s.requireAuth(s.jobUpdate))
	mux.HandleFunc("POST /jobs/{id}/delete", s.requireAuth(s.jobDelete))
	mux.HandleFunc("POST /jobs/{id}/backup", s.requireAuth(s.jobBackup))

	mux.HandleFunc("GET /backups", s.requireAuth(s.backupsList))
	mux.HandleFunc("POST /backups/{id}/restore", s.requireAuth(s.backupRestore))
	mux.HandleFunc("POST /backups/{id}/delete", s.requireAuth(s.backupDelete))
	mux.HandleFunc("GET /backups/{id}/download", s.requireAuth(s.backupDownload))

	mux.HandleFunc("GET /settings", s.requireAuth(s.settingsForm))
	mux.HandleFunc("POST /settings", s.requireAuth(s.settingsSave))
	mux.HandleFunc("POST /logout", s.requireAuth(s.logout))

	static, err := fs.Sub(files, "static")
	if err != nil {
		panic("web: embedded static subtree: " + err.Error())
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	return s
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

// pageData is the template context for full pages.
type pageData struct {
	Title string
	CSRF  string
	Msg   string
	Err   string
	Data  any
}

var funcMap = template.FuncMap{
	// opt renders the selected attribute for <option> dropdowns.
	"opt": func(value, current string) string {
		if value == current {
			return "selected"
		}
		return ""
	},
}

// render parses base layout + fragment + the named page and writes it.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	t, err := template.New("base.html").Funcs(funcMap).ParseFS(files,
		"templates/base.html", "templates/fragment_jobs.html", "templates/"+name)
	if err != nil {
		slog.Error("web: parse templates", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "base.html", data); err != nil {
		slog.Error("web: render template", "template", name, "err", err)
	}
}

// page renders an authenticated page, injecting session + flash context.
func (s *Server) page(w http.ResponseWriter, r *http.Request, name, title string, status int, data any) {
	s.render(w, status, name, pageData{
		Title: title,
		CSRF:  sessionFrom(r).CSRF,
		Msg:   r.URL.Query().Get("msg"),
		Err:   r.URL.Query().Get("err"),
		Data:  data,
	})
}

// renderFragment writes only the named fragment template (htmx poll target).
func (s *Server) renderFragment(w http.ResponseWriter, tmpl, execName string, data any) {
	t, err := template.New(tmpl).ParseFS(files, "templates/"+tmpl)
	if err != nil {
		slog.Error("web: parse fragment", "template", tmpl, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, execName, data); err != nil {
		slog.Error("web: render fragment", "template", tmpl, "err", err)
	}
}

// redirectTo redirects with a stateless flash message via query params.
func redirectTo(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	q := url.Values{}
	if msg != "" {
		q.Set("msg", clip(msg, 300))
	}
	if errMsg != "" {
		q.Set("err", clip(errMsg, 300))
	}
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// clip caps flash text so error tails can't blow up redirect URLs.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// pathID parses the {id} path segment; bad values become 404s.
func pathID(r *http.Request) (int64, bool) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		return 0, false
	}
	return id, true
}

// parseID parses a decimal entity id from a path or query segment.
func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id %q", s)
	}
	return id, nil
}
