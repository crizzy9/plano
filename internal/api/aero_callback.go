package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"mime"
	"net/http"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) aeroCallback(w http.ResponseWriter, r *http.Request) {
	// FlightAware supports HTTPS Basic auth or an opaque token on the callback
	// URL. Prefer Basic to keep credentials out of proxy access log URLs.
	user, secret, basic := r.BasicAuth()
	if !basic {
		secret = r.URL.Query().Get("token")
	}
	want, got := sha256.Sum256(s.AeroCallbackKey), sha256.Sum256([]byte(secret))
	if len(s.AeroCallbackKey) == 0 || basic && user != "plano" || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="Plano AeroAPI callback"`)
		respond(w, http.StatusUnauthorized, map[string]string{"error": "valid callback credential required"})
		return
	}
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		respond(w, http.StatusUnsupportedMediaType, map[string]string{"error": "expected application/json"})
		return
	}
	alert, err := feeds.DecodeAeroAlert(r.Body)
	if errors.Is(err, feeds.ErrNotFound) {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		s.failure(w, err)
		return
	}
	if err := s.Store.QueueAeroRefresh(r.Context(), alert.FlightID, time.Now()); err != nil {
		s.failure(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
