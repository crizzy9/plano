package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) radar(w http.ResponseWriter, r *http.Request, owner string) error {
	id := r.PathValue("id")
	if !flightID.MatchString(id) {
		return errInvalid
	}
	query := r.URL.Query()
	provider := query.Get("provider")
	if (provider != "" || query.Get("providerAfter") != "" || query.Get("providerRevision") != "" || query.Get("receiverRevision") != "") && provider != "1" {
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
	receiverRevision, err := parseReceiverRevision(query.Get("receiverRevision"))
	if err != nil {
		return err
	}
	const pageSize = 256
	result := make(map[string]any)
	var positions []feeds.Position
	if provider == "1" {
		providerLimit := pageSize
		if providerRevision != nil {
			// A replacement can fill the client's entire visible history.
			providerLimit = 512
		}
		page, err := s.Store.RadarPage(r.Context(), owner, id, after, receiverRevision, providerAfter, providerRevision, pageSize, providerLimit)
		if err != nil {
			return err
		}
		positions = page.Positions
		if page.ReceiverRevision != nil {
			result["receiverRevision"] = strconv.FormatInt(*page.ReceiverRevision, 10)
		}
		result["providerPositions"] = page.ProviderPositions
		if page.ProviderRevision != nil {
			result["providerRevision"] = page.ProviderRevision.Format(time.RFC3339Nano)
		}
		if len(page.ProviderPositions) > 0 {
			result["providerCursor"] = page.ProviderPositions[len(page.ProviderPositions)-1].At.UTC().Format(time.RFC3339Nano)
		}
	} else {
		positions, err = s.Store.RadarPositions(r.Context(), owner, id, after, pageSize)
		if err != nil {
			return err
		}
	}
	result["positions"] = positions
	if len(positions) > 0 {
		result["cursor"] = positions[len(positions)-1].At.UTC().Format(time.RFC3339Nano)
	}
	respond(w, 200, result)
	return nil
}

func parseReceiverRevision(value string) (*int64, error) {
	if value == "" {
		return nil, nil
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 0 || strconv.FormatInt(revision, 10) != value {
		return nil, errInvalid
	}
	return &revision, nil
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
