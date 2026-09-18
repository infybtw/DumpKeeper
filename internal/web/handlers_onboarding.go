package web

import (
	"errors"
	"log/slog"
	"net/http"

	"dumpkeeper/internal/db"
)

// onboardingPending reports whether the first-run guide should be shown.
// It stays pending until the user finishes or skips it once, which stores
// SettingOnboardingDone; a database error never blocks the UI.
func (s *Server) onboardingPending() bool {
	value, err := s.db.GetSetting(db.SettingOnboardingDone)
	if err != nil {
		return errors.Is(err, db.ErrNotFound)
	}
	return value != "1"
}

// onboardingDismiss records that the guide was finished or skipped so it is
// not shown again. Called by fetch when the dialog closes.
func (s *Server) onboardingDismiss(w http.ResponseWriter, r *http.Request) {
	if err := s.db.SetSetting(db.SettingOnboardingDone, "1"); err != nil {
		slog.Warn("save onboarding state", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// onboardingReset clears the dismissal so the guide shows on the next page
// load (Settings → "Show setup guide again").
func (s *Server) onboardingReset(w http.ResponseWriter, r *http.Request) {
	if err := s.db.SetSetting(db.SettingOnboardingDone, ""); err != nil {
		s.redirectTo(w, r, "/settings", "", "Could not reset the setup guide: "+err.Error())
		return
	}
	// The cleared flag makes page() render the guide dialog on redirect.
	s.redirectTo(w, r, "/settings", "", "")
}
