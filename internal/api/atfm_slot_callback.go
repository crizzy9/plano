package api

import (
	"bytes"
	"net/http"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) atfmSlotCallback(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	body, ok := readSignedCallback(w, r, now, s.AdvisoryIngressKey, feeds.MaxATFMSlotBytes, "ATFM slot")
	if !ok {
		return
	}
	slot, err := feeds.DecodeATFMSlot(bytes.NewReader(body), now)
	if err != nil {
		s.failure(w, err)
		return
	}
	if err := s.Store.PublishATFMSlot(r.Context(), slot, now); err != nil {
		s.failure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
