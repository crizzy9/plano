package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

// applyAeroInbound updates the rotation and forecast of currently tracked
// onward flights. The callback supplies evidence about the inbound only.
func (s *Store) applyAeroInbound(ctx context.Context, providerID string, incoming feeds.Flight, now time.Time) error {
	if providerID == "" || incoming.AeroProviderID() != providerID || incoming.ObservedAt.IsZero() || incoming.ObservedAt.After(now) {
		return nil
	}
	incoming.ID = providerID
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT snapshot,inbound,prediction FROM flights WHERE id IN (`+aeroInboundTargetsSQL+`)`, aeroInboundTargetArgs(providerID, now)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type target struct {
		snapshot, rotation, prediction []byte
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.snapshot, &t.rotation, &t.prediction); err != nil {
			return err
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	changed := false
	for _, t := range targets {
		var flight feeds.Flight
		if err := json.Unmarshal(t.snapshot, &flight); err != nil {
			return err
		}
		if !flightAlertRelevant("delay_prediction", flight) || !linksAeroInbound(flight, incoming) {
			continue
		}
		var previous []feeds.Flight
		if err := json.Unmarshal(t.rotation, &previous); err != nil {
			return err
		}
		if len(previous) != 0 && (previous[0].ID != providerID || !incoming.ObservedAt.After(previous[0].ObservedAt)) {
			continue
		}
		direct := incoming
		if len(previous) != 0 {
			retainAircraftMetadata(&direct, previous[0], "")
		}
		rotation := []feeds.Flight{direct}
		if len(previous) > 1 && connectsAeroInbound(flight, incoming) && sameInboundHistory(previous[0], incoming) {
			rotation = append(rotation, previous[1:]...)
		}
		advisories, deadlines, err := readCurrentFlightAdvisories(ctx, tx, flight, now)
		if err != nil {
			return err
		}
		forecast := PredictWithModel(s.delayModelFor(flight, now), flight, rotation, advisories, now)
		capCachedAdvisoryValidity(forecast, deadlines)
		event, cleared, err := delayForecastTransition(ctx, tx, flight, flight, t.rotation, t.prediction, forecast, now, true)
		if err != nil {
			return err
		}
		rawRotation, err := json.Marshal(rotation)
		if err != nil {
			return err
		}
		rawForecast, err := json.Marshal(forecast)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE flights SET inbound=?,prediction=? WHERE id=?`, string(rawRotation), string(rawForecast), flight.ID); err != nil {
			return err
		}
		changed = true
		if event.Kind != "" || cleared {
			if err := pruneSupersededFlightAlerts(ctx, tx, flight.ID, []string{"delay_prediction"}); err != nil {
				return err
			}
		}
		if event.Kind != "" {
			if err := saveEvent(ctx, tx, flight.ID, event, true); err != nil {
				return err
			}
		}
	}
	if !changed {
		return nil
	}
	return s.commitOutbox(tx)
}

func linksAeroInbound(flight, inbound feeds.Flight) bool {
	return flight.InboundID != "" && inbound.ID == flight.InboundID && inbound.ID != flight.ID && inbound.ID != flight.AeroProviderID()
}

func connectsAeroInbound(flight, inbound feeds.Flight) bool {
	return (inbound.Destination != "" || inbound.DestinationICAO != "") &&
		!flightAlertAirportChanged(inbound.Destination, inbound.DestinationICAO, flight.Origin, flight.OriginICAO) &&
		compatibleInboundTails(flight.TailNumber, inbound.TailNumber)
}

func compatibleInboundTails(a, b string) bool {
	return a == "" || b == "" || strings.EqualFold(a, b)
}

func sameInboundHistory(before, after feeds.Flight) bool {
	return !flightAlertAirportChanged(before.Origin, before.OriginICAO, after.Origin, after.OriginICAO) &&
		compatibleInboundTails(before.TailNumber, after.TailNumber)
}

// keepNewerAeroInbound protects a callback revision that committed while a poll
// was fetching the rotation. Fresh evidence of a different leg still takes
// effect through the ordinary poll path.
func keepNewerAeroInbound(flight feeds.Flight, rotation []feeds.Flight, previousRaw []byte) ([]feeds.Flight, bool, error) {
	if flight.InboundID == "" {
		return rotation, false, nil
	}
	var previous []feeds.Flight
	if err := json.Unmarshal(previousRaw, &previous); err != nil {
		return nil, false, err
	}
	if len(previous) == 0 || !isExactAeroInbound(flight, previous[0]) {
		return rotation, false, nil
	}
	if len(rotation) == 0 || !isExactAeroInbound(flight, rotation[0]) {
		// An empty response has no inbound timestamp of its own. Compare it
		// with the outbound observation taken before the rotation fetch.
		observedAt := flight.ObservedAt
		if len(rotation) != 0 {
			observedAt = rotation[0].ObservedAt
		}
		if !previous[0].ObservedAt.After(observedAt) {
			return rotation, false, nil
		}
		if !connectsAeroInbound(flight, previous[0]) {
			previous = previous[:1]
		}
		return previous, true, nil
	}
	if rotation[0].ObservedAt.After(previous[0].ObservedAt) {
		direct := rotation[0]
		if retainAircraftMetadata(&direct, previous[0], "") {
			rotation = slices.Clone(rotation)
			rotation[0] = direct
		}
		return rotation, false, nil
	}
	if !connectsAeroInbound(flight, previous[0]) || !sameInboundHistory(rotation[0], previous[0]) {
		// A newer authoritative contradiction must not carry the poll's chain.
		return []feeds.Flight{previous[0]}, true, nil
	}
	rotation = slices.Clone(rotation)
	rotation[0] = previous[0]
	return rotation, true, nil
}

func isExactAeroInbound(flight, inbound feeds.Flight) bool {
	return linksAeroInbound(flight, inbound) && inbound.AeroProviderID() == flight.InboundID
}

// retainAircraftMetadata carries established facts onto a newer observation
// of the same verified AeroAPI instance and assigned aircraft. The stored
// provider key is needed when a top-level flight uses a synthetic schedule ID.
func retainAircraftMetadata(next *feeds.Flight, previous feeds.Flight, previousProvider string) bool {
	if previousProvider == "" {
		previousProvider = previous.AeroProviderID()
	}
	if previousProvider == "" || next.AeroProviderID() != previousProvider || next.TailNumber == "" || next.TailNumber != previous.TailNumber {
		return false
	}
	changed := false
	if next.AircraftAge == nil && previous.AircraftAge != nil {
		next.AircraftAge = previous.AircraftAge
		changed = true
	}
	if next.AircraftModel == "" && previous.AircraftModel != "" {
		next.AircraftModel = previous.AircraftModel
		changed = true
	}
	return changed
}

func delayForecastTransition(ctx context.Context, tx *sql.Tx, prior, flight feeds.Flight, previousInbound, previousPrediction []byte,
	forecast *Prediction, decisionAt time.Time, sameSchedule bool) (Event, bool, error) {
	var old *Prediction
	if err := json.Unmarshal(previousPrediction, &old); err != nil {
		return Event{}, false, err
	}
	var inboundExpired bool
	if old != nil && forecast != nil && sameSchedule && usesInboundTiming(old) {
		var rotation []feeds.Flight
		if err := json.Unmarshal(previousInbound, &rotation); err != nil {
			return Event{}, false, err
		}
		// Recovery can replace an expired warning even if the minutes hardly change.
		_, valid := predictionValidity(old, prior, rotation, decisionAt)
		inboundExpired = !valid
	}
	cleared := old != nil && old.DelayMinutes >= flightDelayAlertMinutes &&
		(!sameSchedule || inboundExpired || forecast == nil || forecast.DelayMinutes < flightDelayAlertMinutes ||
			old.DelayMinutes-forecast.DelayMinutes >= flightDelayAlertMinutes)
	if forecast == nil || forecast.DelayMinutes < flightDelayAlertMinutes ||
		(sameSchedule && !inboundExpired && old != nil && forecast.DelayMinutes < old.DelayMinutes+flightDelayAlertMinutes && slices.Equal(forecast.Reasons, old.Reasons)) {
		return Event{}, cleared, nil
	}
	var latest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(e.at_ns) FROM events e
LEFT JOIN event_revisions r ON r.event_id=e.id LEFT JOIN flight_event_revisions f ON f.flight_id=e.flight_id
WHERE e.flight_id=? AND COALESCE(r.revision,0)=COALESCE(f.revision,0) AND json_extract(e.data,'$.kind')='delay_prediction'`, flight.ID).Scan(&latest); err != nil {
		return Event{}, false, err
	}
	if latest.Valid && decisionAt.UnixNano() <= latest.Int64 {
		decisionAt = time.Unix(0, latest.Int64+1)
	}
	return Event{Kind: "delay_prediction", Title: "Departure delay predicted", Body: fmt.Sprintf("%s · %d min later", flight.Number, forecast.DelayMinutes),
		At: decisionAt.UTC(), validUntil: forecast.ValidUntil, advisoryEvidence: forecast.AdvisoryEvidence}, cleared, nil
}
