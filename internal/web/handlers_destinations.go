package web

import (
	"context"
	"dumpkeeper/internal/backup"
	"dumpkeeper/internal/db"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// destinationForm is the display shape of the destination create/edit form.
type destinationForm struct {
	Action    string
	Error     string
	ID        int64
	Name      string
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	UseSSL    bool
}

// IsNew distinguishes create from edit in the template.
func (f destinationForm) IsNew() bool { return f.ID == 0 }

func defaultDestinationForm(action string) destinationForm {
	return destinationForm{Action: action, UseSSL: true}
}

func destinationAction(id int64) string {
	if id == 0 {
		return "/destinations/new"
	}
	return "/destinations/" + strconv.FormatInt(id, 10) + "/edit"
}

func (s *Server) destinationsList(w http.ResponseWriter, r *http.Request) {
	dests, err := s.db.ListDestinations()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.page(w, r, "destinations.html", "Destinations", http.StatusOK, dests)
}

func (s *Server) destinationNewForm(w http.ResponseWriter, r *http.Request) {
	f := defaultDestinationForm("/destinations/new")
	if isHtmx(r) {
		s.destinationModal(w, r, f)
		return
	}
	s.page(w, r, "destination_form.html", "New destination", http.StatusOK, f)
}

// destinationModal writes the destination modal fragment (create and edit).
// Status 200: htmx 2.x does not swap 4xx responses.
func (s *Server) destinationModal(w http.ResponseWriter, r *http.Request, f destinationForm) {
	s.renderModal(w, http.StatusOK, "fragment_destination_form.html", "destination-modal", sessionFrom(r).CSRF, f)
}

func (s *Server) destinationCreate(w http.ResponseWriter, r *http.Request) {
	d, f := s.parseDestinationForm(r, 0)
	if f.Error != "" {
		if isHtmx(r) {
			s.destinationModal(w, r, f)
			return
		}
		s.page(w, r, "destination_form.html", "New destination", http.StatusBadRequest, f)
		return
	}
	created, err := s.db.CreateDestination(d)
	if err != nil {
		f.Error = "Could not save destination: " + err.Error()
		if isHtmx(r) {
			s.destinationModal(w, r, f)
			return
		}
		s.page(w, r, "destination_form.html", "New destination", http.StatusBadRequest, f)
		return
	}
	if isHtmx(r) {
		s.htmxRedirect(w, "/destinations", "Destination "+created.Name+" created.", "")
		return
	}
	s.redirectTo(w, r, "/destinations", "Destination "+created.Name+" created.", "")
}

func (s *Server) destinationEditForm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, err := s.db.GetDestination(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f := destinationForm{
		Action: destinationAction(d.ID), ID: d.ID,
		Name: d.Name, Endpoint: d.Endpoint, Region: d.Region,
		Bucket: d.Bucket, Prefix: d.Prefix,
		AccessKey: d.AccessKey, SecretKey: d.SecretKey, UseSSL: d.UseSSL,
	}
	if isHtmx(r) {
		s.destinationModal(w, r, f)
		return
	}
	s.page(w, r, "destination_form.html", "Edit destination", http.StatusOK, f)
}

func (s *Server) destinationUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, f := s.parseDestinationForm(r, id)
	if f.Error != "" {
		if isHtmx(r) {
			s.destinationModal(w, r, f)
			return
		}
		s.page(w, r, "destination_form.html", "Edit destination", http.StatusBadRequest, f)
		return
	}
	if err := s.db.UpdateDestination(d); err != nil {
		f.Error = "Could not save destination: " + err.Error()
		if isHtmx(r) {
			s.destinationModal(w, r, f)
			return
		}
		s.page(w, r, "destination_form.html", "Edit destination", http.StatusBadRequest, f)
		return
	}
	if isHtmx(r) {
		s.htmxRedirect(w, "/destinations", "Destination "+d.Name+" updated.", "")
		return
	}
	s.redirectTo(w, r, "/destinations", "Destination "+d.Name+" updated.", "")
}

// destinationTest confirms that the saved credentials can access the bucket
// without creating or changing any objects.
func (s *Server) destinationTest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, err := s.db.GetDestination(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := backup.S3Store(d).Check(ctx); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			s.redirectTo(w, r, "/destinations", "", "Destination test timed out after 10 seconds.")
			return
		}
		s.redirectTo(w, r, "/destinations", "", "Could not access bucket for destination "+d.Name+": "+err.Error())
		return
	}
	s.redirectTo(w, r, "/destinations", "Destination "+d.Name+" can access bucket "+d.Bucket+".", "")
}

func (s *Server) destinationDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, err := s.db.GetDestination(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Best-effort cleanup of the objects stored via this destination; the
	// job/backup links cascade afterwards.
	backups, err := s.db.ListBackupsForDestination(d.ID)
	if err != nil {
		slog.Warn("delete destination: list backups", "destination", d.Name, "err", err)
	}
	for _, b := range backups {
		if err := backup.S3Store(d).Delete(r.Context(), b.Filename); err != nil {
			slog.Warn("delete destination: remove object", "destination", d.Name, "object", b.Filename, "err", err)
		}
	}
	if err := s.db.DeleteDestination(d.ID); err != nil {
		s.redirectTo(w, r, "/destinations", "", "Could not delete destination: "+err.Error())
		return
	}
	s.redirectTo(w, r, "/destinations", "Destination "+d.Name+" deleted.", "")
}

// parseDestinationForm reads and validates the destination form;
// f.Error != "" means invalid and the form is pre-filled for re-rendering.
func (s *Server) parseDestinationForm(r *http.Request, id int64) (db.Destination, destinationForm) {
	f := destinationForm{
		ID: id, Action: destinationAction(id),
		Name:      strings.TrimSpace(r.FormValue("name")),
		Endpoint:  strings.TrimSpace(r.FormValue("endpoint")),
		Region:    strings.TrimSpace(r.FormValue("region")),
		Bucket:    strings.TrimSpace(r.FormValue("bucket")),
		Prefix:    strings.TrimSpace(r.FormValue("prefix")),
		AccessKey: strings.TrimSpace(r.FormValue("access_key")),
		SecretKey: r.FormValue("secret_key"),
		UseSSL:    r.FormValue("use_ssl") == "1",
	}
	switch {
	case f.Name == "":
		f.Error = "Name is required."
	case f.Endpoint == "":
		f.Error = "Endpoint is required."
	case f.Bucket == "":
		f.Error = "Bucket is required."
	case f.AccessKey == "":
		f.Error = "Access key is required."
	case f.SecretKey == "":
		f.Error = "Secret key is required."
	}
	if f.Error == "" {
		exists, err := s.db.DestinationNameExists(f.Name, id)
		switch {
		case err != nil:
			f.Error = err.Error()
		case exists:
			f.Error = "A destination with this name already exists."
		}
	}
	return db.Destination{
		ID: id, Name: f.Name, Endpoint: f.Endpoint, Region: f.Region,
		Bucket: f.Bucket, Prefix: f.Prefix,
		AccessKey: f.AccessKey, SecretKey: f.SecretKey, UseSSL: f.UseSSL,
	}, f
}
