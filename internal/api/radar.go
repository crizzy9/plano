package api

import (
	"net/http"
	"time"
)

func (s Server) radar(w http.ResponseWriter, r *http.Request, owner string) error {
	id := r.PathValue("id")
	if !flightID.MatchString(id) {
		return errInvalid
	}
	var after *time.Time
	if cursor := r.URL.Query().Get("after"); cursor != "" {
		instant, err := time.Parse(time.RFC3339Nano, cursor)
		if err != nil || instant.Year() < 2000 || instant.Year() > 2100 {
			return errInvalid
		}
		after = &instant
	}
	positions, err := s.Store.RadarPositions(r.Context(), owner, id, after, 256)
	if err != nil {
		return err
	}
	result := map[string]any{"positions": positions}
	if len(positions) > 0 {
		result["cursor"] = positions[len(positions)-1].At.UTC().Format(time.RFC3339Nano)
	}
	respond(w, 200, result)
	return nil
}
