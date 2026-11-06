package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const providerTrackSchema = `
CREATE TABLE IF NOT EXISTS radar_provider_revisions (
  flight_id TEXT PRIMARY KEY REFERENCES flights(id) ON DELETE CASCADE,
  revision_ns INTEGER NOT NULL,
  scope_ns INTEGER NOT NULL,
  fetched_ns INTEGER NOT NULL
);`

type providerTrackState struct {
	revision, scope, fetched int64
}

func (r providerTrackState) save(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO radar_provider_revisions(flight_id,revision_ns,scope_ns,fetched_ns)
VALUES(?,?,?,?) ON CONFLICT(flight_id) DO UPDATE SET
revision_ns=excluded.revision_ns, scope_ns=excluded.scope_ns, fetched_ns=excluded.fetched_ns`,
		id, r.revision, r.scope, r.fetched)
	return err
}

func sameProviderTrackIdentity(prior, flight feeds.Flight, previousProvider, provider string) bool {
	return previousProvider != "" && previousProvider == provider &&
		prior.Source == "flightaware_aeroapi" && flight.Source == prior.Source &&
		sameProviderTrackAirport(prior.Origin, prior.OriginICAO, flight.Origin, flight.OriginICAO) &&
		sameProviderTrackAirport(prior.Destination, prior.DestinationICAO, flight.Destination, flight.DestinationICAO) &&
		sameOptionalInstant(prior.ScheduledDeparture, flight.ScheduledDeparture) &&
		(prior.Status == "cancelled") == (flight.Status == "cancelled")
}

func sameProviderTrackAirport(a, aICAO, b, bICAO string) bool {
	return !flightAlertAirportChanged(a, aICAO, b, bICAO) && !flightAlertAirportChanged(b, bICAO, a, aICAO)
}

func verifiedProviderTrackMovement(flight feeds.Flight) bool {
	if flight.TailNumber == "" || flight.ScheduledDeparture == nil || flight.ActualTakeoff == nil ||
		flight.ActualTakeoff.After(flight.ObservedAt) ||
		flight.Origin == "" && flight.OriginICAO == "" || flight.Destination == "" && flight.DestinationICAO == "" {
		return false
	}
	switch flight.Status {
	case "airborne", "diverted", "landed", "arrived":
		return true
	default:
		return false
	}
}

// saveOverlappingProviderTrack updates just the radar snapshot after a newer
// flight observation has already won. The caller has locked and verified the
// tracking and movement identity in this transaction.
func saveOverlappingProviderTrack(ctx context.Context, tx *sql.Tx, prior, flight feeds.Flight,
	track []feeds.Position, oldTrack []byte, state providerTrackState, fetchedAt time.Time) error {
	fetched := fetchedAt.UnixNano()
	if fetchedAt.IsZero() || len(track) == 0 || fetched <= state.fetched ||
		flight.ObservedAt.UnixNano() < state.scope ||
		!verifiedProviderTrackMovement(prior) || !verifiedProviderTrackMovement(flight) {
		return nil
	}
	latest := time.Now().Add(30 * time.Second)
	var positions []feeds.Position
	for _, p := range track {
		if p.Source == "flightaware_aeroapi" && !p.At.Before(*prior.ActualTakeoff) && !p.At.After(latest) &&
			p.At.Year() >= 2000 && p.At.Year() <= 2100 &&
			p.Latitude >= -90 && p.Latitude <= 90 && p.Longitude >= -180 && p.Longitude <= 180 {
			positions = append(positions, p)
		}
	}
	if len(positions) == 0 {
		return nil
	}
	if bytes.Contains(oldTrack, []byte(`"operator_readsb"`)) {
		var legacy []feeds.Position
		if err := json.Unmarshal(oldTrack, &legacy); err != nil {
			return err
		}
		for _, p := range legacy {
			if p.Source == "operator_readsb" && !p.At.Before(*prior.ActualTakeoff) && !p.At.After(latest) {
				positions = append(positions, p)
			}
		}
	}
	slices.SortStableFunc(positions, func(a, b feeds.Position) int { return a.At.Compare(b.At) })
	positions = slices.CompactFunc(positions, func(a, b feeds.Position) bool {
		return a.At.Equal(b.At) && a.Latitude == b.Latitude && a.Longitude == b.Longitude
	})
	if len(positions) > 2000 {
		positions = positions[len(positions)-2000:]
	}
	data, err := json.Marshal(positions)
	if err != nil {
		return err
	}
	state.fetched = fetched
	if !bytes.Equal(oldTrack, data) {
		if _, err := tx.ExecContext(ctx, `UPDATE flights SET telemetry=? WHERE id=?`, string(data), prior.ID); err != nil {
			return err
		}
		state.revision = max(state.revision, prior.ObservedAt.UnixNano()) + 1
	}
	if err := state.save(ctx, tx, prior.ID); err != nil {
		return err
	}
	return tx.Commit()
}
