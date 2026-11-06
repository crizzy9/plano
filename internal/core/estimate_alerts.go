package core

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const estimateAlertThreshold = 5 * time.Minute

// An anchor is the estimate at the last emitted change, or the reference
// used since estimates first became available for this phase and airport.
type estimateAlertAnchors struct {
	departure, arrival sql.NullTime
}

func (anchors *estimateAlertAnchors) appendChanges(before, after feeds.Flight, add func(kind, title, body string)) {
	for _, item := range []struct {
		kind, title                  string
		previous, current, scheduled *time.Time
		anchor                       *sql.NullTime
		airportChanged               bool
	}{
		{"etd", "Departure estimate changed", before.EstimatedDeparture, after.EstimatedDeparture, after.ScheduledDeparture,
			&anchors.departure, flightAlertAirportChanged(before.Origin, before.OriginICAO, after.Origin, after.OriginICAO)},
		{"eta", "Arrival estimate changed", before.EstimatedArrival, after.EstimatedArrival, after.ScheduledArrival,
			&anchors.arrival, flightAlertAirportChanged(before.Destination, before.DestinationICAO, after.Destination, after.DestinationICAO)},
	} {
		reference := firstTime(item.previous, item.scheduled)
		if !flightAlertRelevant(item.kind, after) {
			// Preserve the historical event while clearing its push reference.
			*item.anchor = sql.NullTime{}
		} else {
			if item.airportChanged || !flightAlertRelevant(item.kind, before) {
				*item.anchor = sql.NullTime{}
				reference = item.scheduled
			}
			if !item.anchor.Valid {
				// A database from an earlier version can seed from its last snapshot.
				*item.anchor = sql.NullTime{Time: firstTime(reference, item.current).UTC(), Valid: true}
			}
			reference = &item.anchor.Time
		}
		if item.current == nil || reference == nil {
			continue
		}
		difference := item.current.Sub(*reference).Round(time.Minute)
		if difference >= estimateAlertThreshold || difference <= -estimateAlertThreshold {
			add(item.kind, item.title, fmt.Sprintf("%s · %s", after.Number, signedMinutes(difference)))
			if item.anchor.Valid {
				item.anchor.Time = item.current.UTC()
			}
		}
	}
}

func (anchors estimateAlertAnchors) save(ctx context.Context, tx *sql.Tx, flightID string) error {
	if !anchors.departure.Valid && !anchors.arrival.Valid {
		_, err := tx.ExecContext(ctx, `DELETE FROM flight_estimate_anchors WHERE flight_id=?`, flightID)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO flight_estimate_anchors(flight_id,departure,arrival) VALUES(?,?,?)
ON CONFLICT(flight_id) DO UPDATE SET departure=excluded.departure,arrival=excluded.arrival`, flightID, anchors.departure, anchors.arrival)
	return err
}
