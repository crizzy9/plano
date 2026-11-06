package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const (
	trafficRadarRetention = 7 * 24 * time.Hour
	trafficRadarMaxFixes  = 2048
)

const trafficRadarSchema = `
CREATE TABLE IF NOT EXISTS swim_radar_positions (
  flight_id TEXT NOT NULL REFERENCES flights(id) ON DELETE CASCADE,
  at_ns INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(flight_id,at_ns)
);
CREATE INDEX IF NOT EXISTS swim_radar_positions_expiry ON swim_radar_positions(at_ns);
CREATE TABLE IF NOT EXISTS swim_radar_revisions (
  flight_id TEXT PRIMARY KEY REFERENCES flights(id) ON DELETE CASCADE,
  revision_ns INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS eurocontrol_radar_positions (
  flight_id TEXT NOT NULL REFERENCES flights(id) ON DELETE CASCADE,
  at_ns INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(flight_id,at_ns)
);
CREATE INDEX IF NOT EXISTS eurocontrol_radar_positions_expiry ON eurocontrol_radar_positions(at_ns);
CREATE TABLE IF NOT EXISTS eurocontrol_radar_revisions (
  flight_id TEXT PRIMARY KEY REFERENCES flights(id) ON DELETE CASCADE,
  revision_ns INTEGER NOT NULL
);`

// Table names come exclusively from these server-owned stream definitions.
type trafficRadarStream struct {
	source    string
	positions string
	revisions string
}

var (
	swimRadarStream        = trafficRadarStream{feeds.FAASWIM, "swim_radar_positions", "swim_radar_revisions"}
	eurocontrolRadarStream = trafficRadarStream{feeds.Eurocontrol, "eurocontrol_radar_positions", "eurocontrol_radar_revisions"}
)

// RecordSWIMPositions appends fresh airborne fixes for a single verified
// tracked movement. The binding, watermark and append share one transaction.
func (s *Store) RecordSWIMPositions(ctx context.Context, report feeds.RadarPositionReport, now time.Time) error {
	return s.recordTrafficPositions(ctx, report, now, swimRadarStream)
}

// RecordEurocontrolPositions appends fixes using EUROCONTROL's own watermark.
func (s *Store) RecordEurocontrolPositions(ctx context.Context, report feeds.RadarPositionReport, now time.Time) error {
	return s.recordTrafficPositions(ctx, report, now, eurocontrolRadarStream)
}

func (s *Store) recordTrafficPositions(ctx context.Context, report feeds.RadarPositionReport, now time.Time, stream trafficRadarStream) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	var latest int64
	err = tx.QueryRowContext(ctx, `WITH targets AS (SELECT id FROM (`+aeroTargetsSQL+`) LIMIT 2)
SELECT snapshot, MAX(COALESCE((SELECT MAX(p.at_ns) FROM `+stream.positions+` p WHERE p.flight_id=flights.id),0),
  COALESCE((SELECT r.revision_ns FROM `+stream.revisions+` r WHERE r.flight_id=flights.id),0))
FROM flights WHERE id=(SELECT CASE WHEN COUNT(*)=1 THEN MIN(id) END FROM targets)
AND json_extract(snapshot,'$.source')='flightaware_aeroapi' AND observed_ns<=? `+radarFlightFreshness,
		report.ProviderFlightID, report.ProviderFlightID, now.UnixNano(), now.Add(-15*time.Minute).UnixNano(),
		now.Add(-30*time.Minute).Format(time.RFC3339)).Scan(&raw, &latest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var flight feeds.Flight
	if err := json.Unmarshal(raw, &flight); err != nil {
		return err
	}
	if !report.MatchesFlight(flight) {
		return nil
	}
	appended := false
	oldest := now.Add(-feeds.RadarPositionFreshness)
	for _, p := range report.Positions {
		if p.At.Before(oldest) || p.At.After(now) || p.At.Before(report.ActualTakeoff) || p.At.UnixNano() <= latest {
			continue
		}
		p.Source, p.Kind = stream.source, "radar"
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		latest = p.At.UnixNano()
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+stream.positions+`(flight_id,at_ns,data) VALUES(?,?,?)`,
			flight.ID, latest, string(data)); err != nil {
			return err
		}
		appended = true
	}
	if !appended {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+stream.positions+` WHERE flight_id=? AND at_ns<=
(SELECT at_ns FROM `+stream.positions+` WHERE flight_id=? ORDER BY at_ns DESC LIMIT 1 OFFSET ?)`,
		flight.ID, flight.ID, trafficRadarMaxFixes); err != nil {
		return err
	}
	return tx.Commit()
}

// Retire the old scope even if no callback has arrived yet. Revisions also act
// as a floor when a scope later returns to its previous values.
func retireTrafficRadarScope(ctx context.Context, tx *sql.Tx, prior, flight feeds.Flight, previousProvider, provider string) error {
	if prior.ID == "" {
		return nil
	}
	if previousProvider == "" {
		previousProvider = prior.AeroProviderID()
	}
	if (provider == "" || provider == previousProvider) && prior.Source == flight.Source &&
		prior.OriginICAO == flight.OriginICAO && prior.DestinationICAO == flight.DestinationICAO &&
		strings.EqualFold(prior.TailNumber, flight.TailNumber) &&
		sameRadarInstant(prior.ScheduledDeparture, flight.ScheduledDeparture) &&
		sameRadarInstant(prior.ActualTakeoff, flight.ActualTakeoff) {
		return nil
	}
	priorScope := previousProvider != "" && prior.Source == "flightaware_aeroapi" && prior.OriginICAO != "" &&
		prior.DestinationICAO != "" && prior.ScheduledDeparture != nil && prior.ActualTakeoff != nil
	// A provider update can describe a change that precedes a fix we already
	// stored. Advance past the erased track as well as the provider timestamp.
	for _, stream := range [...]trafficRadarStream{swimRadarStream, eurocontrolRadarStream} {
		result, err := tx.ExecContext(ctx, `INSERT INTO `+stream.revisions+`(flight_id,revision_ns)
SELECT ?,MAX(?,COALESCE((SELECT MAX(at_ns) FROM `+stream.positions+` WHERE flight_id=?),0))
WHERE ? OR EXISTS(SELECT 1 FROM `+stream.revisions+` WHERE flight_id=?)
ON CONFLICT(flight_id) DO UPDATE SET revision_ns=MAX(`+stream.revisions+`.revision_ns+1,excluded.revision_ns)`,
			flight.ID, flight.ObservedAt.UnixNano(), flight.ID, priorScope, flight.ID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+stream.positions+` WHERE flight_id=?`, flight.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameRadarInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// RadarCursor selects fixes newer than After for a known stream revision.
type RadarCursor struct {
	After    *time.Time
	Revision *int64
}

func readTrafficRadarPositions(ctx context.Context, tx *sql.Tx, id string, cursor RadarCursor, limit, snapshotLimit int, stream trafficRadarStream) ([]feeds.Position, *int64, error) {
	// The combined page has already checked tracking in this transaction.
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision_ns FROM `+stream.revisions+` WHERE flight_id=?),0)`, id).Scan(&revision); err != nil {
		return nil, nil, err
	}
	if cursor.Revision != nil && *cursor.Revision != revision {
		cursor.After = nil
	}
	cutoff := time.Now().Add(-trafficRadarRetention).UnixNano()
	positions, err := readRadarFixPage(ctx, tx, stream.positions, `WHERE p.flight_id=? AND p.at_ns>?`,
		[]any{id, cutoff}, cursor.After, limit, snapshotLimit)
	return positions, &revision, err
}

func pruneTrafficRadarPositions(ctx context.Context, tx *sql.Tx, now time.Time) error {
	cutoff := now.Add(-trafficRadarRetention).UnixNano()
	for _, stream := range [...]trafficRadarStream{swimRadarStream, eurocontrolRadarStream} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+stream.positions+` WHERE at_ns<=?`, cutoff); err != nil {
			return err
		}
	}
	return nil
}
