package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"dumpkeeper/internal/db"
)

const (
	sessionCookie = "dk_session"
	sessionTTL    = 168 * time.Hour
)

type ctxKey struct{}

// requireAuth enforces session cookie presence, then CSRF on POSTs, then
// hands the session to the handler via context.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost &&
			subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.CSRF)) != 1 {
			http.Error(w, "Forbidden: invalid CSRF token", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	}
}

// sessionFrom returns the session stored by requireAuth (zero value on the
// login page).
func sessionFrom(r *http.Request) db.Session {
	sess, _ := r.Context().Value(ctxKey{}).(db.Session)
	return sess
}

// currentSession resolves the cookie to a live session.
func (s *Server) currentSession(r *http.Request) (db.Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return db.Session{}, false
	}
	sess, err := s.db.GetSession(c.Value)
	if err != nil {
		return db.Session{}, false
	}
	return sess, true
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, http.StatusOK, "")
}

func (s *Server) renderLogin(w http.ResponseWriter, status int, errMsg string) {
	s.render(w, status, "login.html", pageData{Title: "Sign in", Data: struct{ Error string }{errMsg}})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	loginOK := subtle.ConstantTimeCompare([]byte(r.PostFormValue("login")), []byte(s.cfg.Login)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(r.PostFormValue("password")), []byte(s.cfg.Password)) == 1
	if !(loginOK && passOK) {
		slog.Warn("failed login attempt", "remote", r.RemoteAddr)
		s.renderLogin(w, http.StatusUnauthorized, "Invalid login or password.")
		return
	}
	if err := s.db.DeleteExpiredSessions(); err != nil {
		slog.Warn("purge expired sessions", "err", err)
	}
	token, err := randomHex()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	csrf, err := randomHex()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	expires := db.FormatTime(time.Now().Add(sessionTTL))
	if err := s.db.CreateSession(token, csrf, expires); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.db.DeleteSession(sessionFrom(r).Token); err != nil {
		slog.Warn("delete session on logout", "err", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// randomHex returns 32 random bytes hex-encoded (session token and CSRF token).
func randomHex() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
