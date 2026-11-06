package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const rotationRadarSchema = `
CREATE TABLE IF NOT EXISTS rotation_radar_positions (
  parent_id TEXT NOT NULL REFERENCES flights(id) ON DELETE CASCADE,
  instance_key TEXT NOT NULL, at_ns INTEGER NOT NULL, data TEXT NOT NULL,
  PRIMARY KEY(parent_id,instance_key,at_ns)
);
CREATE INDEX IF NOT EXISTS rotation_radar_positions_expiry ON rotation_radar_positions(at_ns);
`

type rotationRadarLeg struct {
	parentID, instanceKey string
	flight                feeds.Flight
}

type rotationRadarSample struct {
	leg      rotationRadarLeg
	position feeds.Position
}

func eligibleRotationRadarLegs(ctx context.Context, db radarRowsQueryer, now time.Time) ([]rotationRadarLeg, error) {
	rows, err := db.QueryContext(ctx, `SELECT f.snapshot,json_extract(f.inbound,'$[0]'),COALESCE(k.provider_id,'')
FROM flights f LEFT JOIN aero_instance_keys k ON k.flight_id=f.id
WHERE f.observed_ns BETWEEN ? AND ? AND json_extract(f.inbound,'$[0].status')='airborne'
AND EXISTS(SELECT 1 FROM tracked WHERE flight_id=f.id)`, now.Add(-15*time.Minute).UnixNano(), now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var legs []rotationRadarLeg
	for rows.Next() {
		var rawParent, rawLeg []byte
		var providerID string
		if err := rows.Scan(&rawParent, &rawLeg, &providerID); err != nil {
			return nil, err
		}
		var parent, leg feeds.Flight
		if err := json.Unmarshal(rawParent, &parent); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(rawLeg, &leg); err != nil {
			return nil, err
		}
		if key := rotationRadarKey(parent, providerID, leg, now); key != "" {
			legs = append(legs, rotationRadarLeg{parentID: parent.ID, instanceKey: key, flight: leg})
		}
	}
	return legs, rows.Err()
}

// rotationRadarKey verifies the saved relationship and separates tracks across
// a provider remap, aircraft reassignment, or corrected inbound movement.
func rotationRadarKey(parent feeds.Flight, providerID string, leg feeds.Flight, now time.Time) string {
	cutoff := now.Add(-15 * time.Minute)
	if !flightBeforeDeparture(parent) || parent.ObservedAt.Before(cutoff) || parent.ObservedAt.After(now) ||
		leg.ID == "" || leg.ID == parent.ID || providerID != "" && leg.ID == providerID ||
		leg.Status != "airborne" || leg.ObservedAt.Before(cutoff) || leg.ObservedAt.After(now) ||
		leg.ActualTakeoff == nil || leg.ActualTakeoff.IsZero() || leg.ActualTakeoff.After(now) ||
		leg.ActualLanding != nil || leg.ActualArrival != nil || strings.TrimSpace(leg.TailNumber) == "" {
		return ""
	}
	if !connectsAeroInbound(parent, leg) ||
		!linksAeroInbound(parent, leg) && !strings.EqualFold(parent.TailNumber, leg.TailNumber) {
		return ""
	}
	if parent.InboundID != "" && parent.InboundID != leg.ID && leg.ObservedAt.Before(parent.ObservedAt) {
		// A partial rotation can still contain an older leg.
		return ""
	}
	if arrival, _ := inboundArrival(leg); arrival != nil {
		departure := firstTime(parent.ScheduledDeparture, parent.EstimatedDeparture)
		if arrival.Before(*leg.ActualTakeoff) || arrival.Before(now.Add(-30*time.Minute)) ||
			departure != nil && !inboundChronologyCompatible(leg, *arrival, *departure) {
			return ""
		}
	}
	fields := []string{
		parent.ID, providerID, parent.Source, parent.InboundID, strings.ToUpper(parent.TailNumber),
		parent.Origin, parent.OriginICAO, parent.Destination, parent.DestinationICAO,
		leg.ID, leg.Source, strings.ToUpper(leg.TailNumber), leg.ActualTakeoff.UTC().Format(time.RFC3339Nano),
		leg.Number, leg.ICAONumber, leg.ATCIdent, leg.Origin, leg.OriginICAO, leg.Destination, leg.DestinationICAO,
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%q", fields))))
}

// Candidates are selected and matched while this write transaction is open.
func recordRotationRadarSamples(ctx context.Context, tx *sql.Tx, samples []rotationRadarSample) error {
	for _, sample := range samples {
		if sample.leg.flight.ActualTakeoff == nil || sample.position.At.Before(*sample.leg.flight.ActualTakeoff) {
			continue
		}
		data, err := json.Marshal(sample.position)
		if err != nil {
			return err
		}
		at := sample.position.At.UnixNano()
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO rotation_radar_positions(parent_id,instance_key,at_ns,data)
SELECT ?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM rotation_radar_positions WHERE parent_id=? AND instance_key=? AND at_ns>=?)`,
			sample.leg.parentID, sample.leg.instanceKey, at, string(data), sample.leg.parentID, sample.leg.instanceKey, at); err != nil {
			return err
		}
	}
	return nil
}

// TrackedRotationRadar reads the saved leg and its receiver fixes together so a
// concurrent reassignment cannot pair the current leg with a previous track.
func (s *Store) TrackedRotationRadar(ctx context.Context, owner, parent, id string, now time.Time) (feeds.Flight, bool, []feeds.Position, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return feeds.Flight{}, false, nil, err
	}
	defer tx.Rollback()
	leg, err := readTrackedRotationLeg(ctx, tx, owner, parent, id)
	if err != nil || !leg.immediate {
		return leg.flight, leg.immediate, nil, err
	}
	key := rotationRadarKey(leg.onward, leg.providerID, leg.flight, now)
	if key == "" {
		return leg.flight, true, nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT data FROM rotation_radar_positions
WHERE parent_id=? AND instance_key=? AND at_ns<=? ORDER BY at_ns DESC LIMIT 256`, parent, key, now.UnixNano())
	if err != nil {
		return feeds.Flight{}, false, nil, err
	}
	defer rows.Close()
	var positions []feeds.Position
	for rows.Next() {
		var raw []byte
		var position feeds.Position
		if err := rows.Scan(&raw); err != nil {
			return feeds.Flight{}, false, nil, err
		}
		if err := json.Unmarshal(raw, &position); err != nil {
			return feeds.Flight{}, false, nil, err
		}
		positions = append(positions, position)
	}
	if err := rows.Err(); err != nil {
		return feeds.Flight{}, false, nil, err
	}
	slices.Reverse(positions)
	return leg.flight, true, positions, nil
}
