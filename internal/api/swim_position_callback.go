package api

import (
	"bytes"
	"net/http"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) swimPositionCallback(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	body, ok := readSignedCallback(w, r, now, s.SWIMPositionKey, feeds.MaxSWIMPositionBytes, "SWIM position")
	if !ok {
		return
	}
	report, err := feeds.DecodeSWIMPositionReport(bytes.NewReader(body), now)
	if err != nil {
		s.failure(w, errInvalid)
		return
	}
	if err := s.Store.RecordSWIMPositions(r.Context(), report, now); err != nil {
		s.failure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
