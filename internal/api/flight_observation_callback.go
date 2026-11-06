package api

import (
	"bytes"
	"errors"
	"net/http"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) flightObservationCallback(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	body, ok := readSignedCallback(w, r, now, s.FlightObservationKey, feeds.MaxFlightObservationBytes, "flight observation")
	if !ok {
		return
	}
	flight, err := feeds.DecodeFlightObservation(bytes.NewReader(body), now)
	if errors.Is(err, feeds.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.failure(w, errInvalid)
		return
	}
	if err := s.Store.IngestAeroObservation(r.Context(), flight, now); err != nil {
		s.failure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
