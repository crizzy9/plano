package api

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) radarPositionCallback(key []byte, source string, record func(context.Context, feeds.RadarPositionReport, time.Time) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()
		body, ok := readSignedCallback(w, r, now, key, feeds.MaxRadarPositionBytes, "radar position")
		if !ok {
			return
		}
		report, err := feeds.DecodeRadarPositionReport(bytes.NewReader(body), now, source)
		if err != nil {
			s.failure(w, errInvalid)
			return
		}
		if err := record(r.Context(), report, now); err != nil {
			s.failure(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
