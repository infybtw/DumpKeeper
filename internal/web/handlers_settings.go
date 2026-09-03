package web

import (
	"log/slog"
	"net/http"
	"strings"
)

// settingsKeys are the global S3 settings persisted on the settings page.
var settingsKeys = []string{
	"s3_endpoint", "s3_region", "s3_bucket", "s3_prefix",
	"s3_access_key", "s3_secret_key",
}

func (s *Server) settingsForm(w http.ResponseWriter, r *http.Request) {
	st, err := s.db.GetSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.page(w, r, "settings.html", "Settings", http.StatusOK, struct {
		S map[string]string
	}{st})
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	vals := make(map[string]string, len(settingsKeys)+1)
	for _, k := range settingsKeys {
		vals[k] = strings.TrimSpace(r.FormValue(k))
	}
	if r.FormValue("s3_use_ssl") == "1" {
		vals["s3_use_ssl"] = "1"
	} else {
		vals["s3_use_ssl"] = "0"
	}
	if err := s.db.SaveSettings(vals); err != nil {
		slog.Error("save settings", "err", err)
		redirectTo(w, r, "/settings", "", "Could not save settings: "+err.Error())
		return
	}
	redirectTo(w, r, "/settings", "Settings saved.", "")
}
