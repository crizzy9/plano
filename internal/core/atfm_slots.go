package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

const atfmSlotRetention = 48 * time.Hour

const atfmSlotSchema = `
CREATE TABLE IF NOT EXISTS atfm_slots (
  provider_id TEXT NOT NULL, source TEXT NOT NULL, origin TEXT NOT NULL, destination TEXT NOT NULL,
  scheduled_ns INTEGER NOT NULL, observed_ns INTEGER NOT NULL, expires_ns INTEGER NOT NULL,
  alert_revision TEXT NOT NULL, data TEXT NOT NULL,
  PRIMARY KEY(provider_id,source,origin,destination,scheduled_ns)
);
CREATE INDEX IF NOT EXISTS atfm_slots_observation ON atfm_slots(observed_ns);
CREATE TABLE IF NOT EXISTS atfm_scope_floors (
  flight_id TEXT PRIMARY KEY REFERENCES flights(id) ON DELETE CASCADE, changed_ns INTEGER NOT NULL
);`

// Like aeroTargetsSQL, prefer the verified mapping over a legacy exact ID.
const atfmProviderKey = `COALESCE(k.provider_id,CASE WHEN json_extract(f.snapshot,'$.source')='flightaware_aeroapi' THEN f.id END)`

type ATFMSlotView struct {
	Source     string    `json:"source"`
	TakeoffAt  time.Time `json:"takeoffAt"`
	ObservedAt time.Time `json:"observedAt"`
	ValidUntil time.Time `json:"validUntil"`
	Reason     string    `json:"reason,omitempty"`
}

// PublishATFMSlot stores the provider revision and any tracked-owner alerts in
// the same transaction. Withdrawals retain their observation watermark.
func (s *Store) PublishATFMSlot(ctx context.Context, slot feeds.ATFMSlot, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	key := []any{slot.ProviderFlightID, slot.Source, slot.OriginICAO, slot.DestinationICAO, slot.ScheduledDeparture.UnixNano()}
	var oldRaw []byte
	var oldObserved int64
	var revision string
	err = tx.QueryRowContext(ctx, `SELECT data,observed_ns,alert_revision FROM atfm_slots
WHERE provider_id=? AND source=? AND origin=? AND destination=? AND scheduled_ns=?`, key...).Scan(&oldRaw, &oldObserved, &revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if oldObserved > slot.ObservedAt.UnixNano() {
		return nil
	}
	var previous feeds.ATFMSlot
	if oldRaw != nil {
		if err := json.Unmarshal(oldRaw, &previous); err != nil {
			return err
		}
	}
	tied := oldObserved == slot.ObservedAt.UnixNano()
	if tied && (slot.Status != feeds.ATFMWithdrawn || previous.Status != feeds.ATFMAssigned) {
		return nil
	}
	flights, err := atfmSlotTargets(ctx, tx, slot)
	if err != nil {
		return err
	}
	// Once a scope is verified, keep accepting its revisions while nobody
	// tracks it. A later retrack must see the latest state for that scope.
	if len(flights) == 0 && oldRaw == nil {
		return nil
	}
	changed := previous.Status != slot.Status || previous.Reason != slot.Reason || !sameATFMTakeoff(previous.TakeoffAt, slot.TakeoffAt)
	if changed {
		revision = hash(strings.Join([]string{slot.ProviderFlightID, slot.Source, slot.OriginICAO, slot.DestinationICAO,
			slot.ScheduledDeparture.UTC().Format(time.RFC3339Nano), slot.ObservedAt.UTC().Format(time.RFC3339Nano), slot.Status}, "\x00"))
	}
	payload, err := json.Marshal(slot)
	if err != nil {
		return err
	}
	validUntil := slot.ValidUntil()
	if _, err := tx.ExecContext(ctx, `INSERT INTO atfm_slots(provider_id,source,origin,destination,scheduled_ns,observed_ns,expires_ns,alert_revision,data)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(provider_id,source,origin,destination,scheduled_ns) DO UPDATE SET
observed_ns=excluded.observed_ns,expires_ns=excluded.expires_ns,alert_revision=excluded.alert_revision,data=excluded.data`,
		append(key, slot.ObservedAt.UnixNano(), validUntil.UnixNano(), revision, string(payload))...); err != nil {
		return err
	}
	if !changed {
		return tx.Commit()
	}
	for _, flight := range flights {
		msg := atfmAlertMessage(slot, flight, revision, validUntil)
		if tied {
			// The withdrawal wins a source timestamp tie and must also sort
			// after the assignment in the notification's revision order.
			msg.Timestamp = msg.Timestamp.Add(time.Nanosecond)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE flight_id=? AND done=0
AND json_extract(message,'$.AlertKind')=? AND json_extract(message,'$.ATFMRevision') IS NOT NULL`, flight.ID, msg.AlertKind); err != nil {
			return err
		}
		if !validUntil.After(now) || slot.Status == feeds.ATFMWithdrawn &&
			(previous.Status != feeds.ATFMAssigned || !previous.ValidUntil().After(now)) {
			continue
		}
		if err := enqueue(ctx, tx, flight.ID, msg, `SELECT d.id FROM devices d JOIN tracked t ON t.owner=d.owner
WHERE t.flight_id=? AND d.kind='alert'`, flight.ID); err != nil {
			return err
		}
	}
	return s.commitOutbox(tx)
}

func sameATFMTakeoff(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func atfmSlotTargets(ctx context.Context, tx *sql.Tx, slot feeds.ATFMSlot) ([]feeds.Flight, error) {
	rows, err := tx.QueryContext(ctx, `SELECT f.snapshot FROM flights f LEFT JOIN atfm_scope_floors floor ON floor.flight_id=f.id
WHERE f.id IN (`+aeroTargetsSQL+`) AND COALESCE(floor.changed_ns,0)<?`, slot.ProviderFlightID, slot.ProviderFlightID, slot.ObservedAt.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var flights []feeds.Flight
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var flight feeds.Flight
		if err := json.Unmarshal(raw, &flight); err != nil {
			return nil, err
		}
		if slot.MatchesFlight(flight) {
			flights = append(flights, flight)
		}
	}
	return flights, rows.Err()
}

func atfmAlertMessage(slot feeds.ATFMSlot, flight feeds.Flight, revision string, validUntil time.Time) push.Message {
	title, kind := "FAA runway release time", "faa_edct"
	if slot.Source == feeds.Eurocontrol {
		title, kind = "EUROCONTROL takeoff slot", "eurocontrol_ctot"
	}
	body := flight.Number + " · The takeoff slot was withdrawn."
	if slot.TakeoffAt != nil {
		body = flight.Number + " · Assigned takeoff " + slot.TakeoffAt.UTC().Format("Jan 2 at 15:04 UTC") + "."
	}
	if slot.Reason != "" {
		body += " " + slot.Reason
	}
	return push.Message{Kind: push.Alert, FlightID: flight.ID, AlertKind: kind, Title: title, Body: truncate(body, 180),
		Timestamp: slot.ObservedAt, ExpiresAt: validUntil, CollapseID: hash(flight.ID + "\x00" + revision),
		APNsCollapseID: hash(flight.ID + "\x00atfm\x00" + slot.Source), ATFMRevision: revision}
}

// ATFMSlotsForFlight checks the tracking grant and snapshot revision again so
// a concurrent change cannot expose data from a different operation.
func (s *Store) ATFMSlotsForFlight(ctx context.Context, owner string, flight feeds.Flight, now time.Time) ([]ATFMSlotView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.data FROM flights f JOIN tracked t ON t.flight_id=f.id
LEFT JOIN aero_instance_keys k ON k.flight_id=f.id LEFT JOIN atfm_scope_floors floor ON floor.flight_id=f.id
JOIN atfm_slots a ON a.provider_id=`+atfmProviderKey+`
WHERE f.id=? AND t.owner=? AND f.observed_ns=? AND a.expires_ns>? AND a.observed_ns<=? AND a.observed_ns>COALESCE(floor.changed_ns,0)
ORDER BY a.source`, flight.ID, owner, flight.ObservedAt.UnixNano(), now.UnixNano(), now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slots []ATFMSlotView
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var slot feeds.ATFMSlot
		if err := json.Unmarshal(raw, &slot); err != nil {
			return nil, err
		}
		if slot.Status == feeds.ATFMAssigned && slot.TakeoffAt != nil && slot.MatchesFlight(flight) {
			slots = append(slots, ATFMSlotView{Source: slot.Source, TakeoffAt: *slot.TakeoffAt, ObservedAt: slot.ObservedAt,
				ValidUntil: slot.ValidUntil(), Reason: slot.Reason})
		}
	}
	return slots, rows.Err()
}

func (s *Store) atfmAlertCurrent(ctx context.Context, job deliveryJob, now time.Time) (bool, error) {
	var snapshot, raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT f.snapshot,a.data FROM outbox o JOIN flights f ON f.id=o.flight_id
JOIN tracked t ON t.flight_id=f.id AND t.owner=? LEFT JOIN aero_instance_keys k ON k.flight_id=f.id
LEFT JOIN atfm_scope_floors floor ON floor.flight_id=f.id
JOIN atfm_slots a ON a.provider_id=`+atfmProviderKey+` AND a.alert_revision=?
WHERE o.id=? AND o.device_id=? AND o.lease_until=? AND o.done=0 AND a.expires_ns>?
AND a.observed_ns<=? AND a.observed_ns>COALESCE(floor.changed_ns,0)`,
		job.owner, job.message.ATFMRevision, job.id, job.deviceID, job.lease, now.UnixNano(), now.UnixNano()).Scan(&snapshot, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var flight feeds.Flight
	if err := json.Unmarshal(snapshot, &flight); err != nil {
		return false, err
	}
	var slot feeds.ATFMSlot
	if err := json.Unmarshal(raw, &slot); err != nil {
		return false, err
	}
	return slot.MatchesFlight(flight), nil
}

// A schedule can move away and later return to the same published instant.
// Require a fresh slot observation after the previous scope stops matching.
func retireATFMSlotScope(ctx context.Context, tx *sql.Tx, prior, flight feeds.Flight, providerChanged bool) error {
	if prior.ScheduledDeparture == nil || prior.OriginICAO == "" || prior.DestinationICAO == "" {
		return nil
	}
	previous := feeds.ATFMSlot{OriginICAO: prior.OriginICAO, DestinationICAO: prior.DestinationICAO, ScheduledDeparture: *prior.ScheduledDeparture}
	if !previous.MatchesFlight(prior) || !providerChanged && previous.MatchesFlight(flight) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO atfm_scope_floors(flight_id,changed_ns) VALUES(?,?)
ON CONFLICT(flight_id) DO UPDATE SET changed_ns=excluded.changed_ns`, flight.ID, flight.ObservedAt.UnixNano()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE flight_id=? AND done=0
AND json_extract(message,'$.ATFMRevision') IS NOT NULL`, flight.ID)
	return err
}
