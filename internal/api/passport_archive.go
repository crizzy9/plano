package api

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/itinerary"
)

const maxPassportCSVBytes = 512 << 10

var passportArchiveID = regexp.MustCompile(`^archive-[0-9a-f]{64}$`)

func (s Server) importPassportArchive(w http.ResponseWriter, r *http.Request, owner string) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/csv" {
		respond(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Upload a Flighty CSV export"})
		return nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPassportCSVBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		respond(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "Flighty CSV must be 512 KiB or smaller"})
		return nil
	}
	if err != nil {
		return errInvalid
	}
	parsed, err := itinerary.PassportCSV(bytes.NewReader(body), time.Now())
	if err != nil {
		respond(w, http.StatusUnprocessableEntity, map[string]string{"error": "Use a valid Flighty export with at most 1,000 rows"})
		return nil
	}
	added, duplicates, err := s.Store.ImportPassportArchive(r.Context(), owner, parsed.Flights)
	if errors.Is(err, core.ErrPassportArchiveFull) {
		respond(w, http.StatusConflict, map[string]string{"error": "Passport can store up to 10,000 imported flights"})
		return nil
	}
	if err != nil {
		return err
	}
	respond(w, http.StatusOK, struct {
		Added      int `json:"added"`
		Duplicates int `json:"duplicates"`
		Skipped    int `json:"skipped"`
	}{added, duplicates, parsed.Skipped})
	return nil
}

func (s Server) deletePassportArchive(w http.ResponseWriter, r *http.Request, owner string) error {
	id := r.PathValue("id")
	if !passportArchiveID.MatchString(id) {
		return errInvalid
	}
	if err := s.Store.DeletePassportArchive(r.Context(), owner, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
