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
	query := r.URL.Query()
	provider := query.Get("provider")
	if (provider != "" || query.Get("providerAfter") != "" || query.Get("providerRevision") != "") && provider != "1" {
		return errInvalid
	}
	after, err := parseRadarCursor(query.Get("after"))
	if err != nil {
		return err
	}
	providerAfter, err := parseRadarCursor(query.Get("providerAfter"))
	if err != nil {
		return err
	}
	providerRevision, err := parseRadarCursor(query.Get("providerRevision"))
	if err != nil {
		return err
	}
	const pageSize = 256
	positions, err := s.Store.RadarPositions(r.Context(), owner, id, after, pageSize)
	if err != nil {
		return err
	}
	result := map[string]any{"positions": positions}
	if len(positions) > 0 {
		result["cursor"] = positions[len(positions)-1].At.UTC().Format(time.RFC3339Nano)
	}
	if provider == "1" {
		providerLimit := pageSize
		if providerRevision != nil {
			// A replacement can fill the client's entire visible history.
			providerLimit = 512
		}
		providerPositions, revision, err := s.Store.RadarProviderPositions(r.Context(), owner, id, providerAfter, providerRevision, providerLimit)
		if err != nil {
			return err
		}
		result["providerPositions"] = providerPositions
		if revision != nil {
			result["providerRevision"] = revision.Format(time.RFC3339Nano)
		}
		if len(providerPositions) > 0 {
			result["providerCursor"] = providerPositions[len(providerPositions)-1].At.UTC().Format(time.RFC3339Nano)
		}
	}
	respond(w, 200, result)
	return nil
}

func parseRadarCursor(cursor string) (*time.Time, error) {
	if cursor == "" {
		return nil, nil
	}
	instant, err := time.Parse(time.RFC3339Nano, cursor)
	if err != nil || instant.Year() < 2000 || instant.Year() > 2100 {
		return nil, errInvalid
	}
	return &instant, nil
}
