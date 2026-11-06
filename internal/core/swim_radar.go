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
	swimRadarRetention = 7 * 24 * time.Hour
	swimRadarMaxFixes  = 2048
)

const swimRadarSchema = `
CREATE TABLE IF NOT EXISTS swim_radar_positions (
  flight_id TEXT NOT NULL REFERENCES flights(id) ON DELETE CASCADE,
  at_ns INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(flight_id,at_ns)
);
CREATE INDEX IF NOT EXISTS swim_radar_positions_expiry ON swim_radar_positions(at_ns);
CREATE TABLE IF NOT EXISTS swim_radar_revisions (
  flight_id TEXT PRIMARY KEY REFERENCES flights(id) ON DELETE CASCADE,
  revision_ns INTEGER NOT NULL
);`

// RecordSWIMPositions appends fresh airborne fixes for a single verified
// tracked movement. The binding, watermark and append share one transaction.
func (s *Store) RecordSWIMPositions(ctx context.Context, report feeds.SWIMPositionReport, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	var latest int64
	err = tx.QueryRowContext(ctx, `WITH targets AS (SELECT id FROM (`+aeroTargetsSQL+`) LIMIT 2)
SELECT snapshot, MAX(COALESCE((SELECT MAX(p.at_ns) FROM swim_radar_positions p WHERE p.flight_id=flights.id),0),
  COALESCE((SELECT r.revision_ns FROM swim_radar_revisions r WHERE r.flight_id=flights.id),0))
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
	oldest := now.Add(-feeds.SWIMPositionFreshness)
	for _, p := range report.Positions {
		if p.At.Before(oldest) || p.At.After(now) || p.At.Before(report.ActualTakeoff) || p.At.UnixNano() <= latest {
			continue
		}
		p.Source, p.Kind = feeds.FAASWIM, "radar"
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		latest = p.At.UnixNano()
		if _, err := tx.ExecContext(ctx, `INSERT INTO swim_radar_positions(flight_id,at_ns,data) VALUES(?,?,?)`,
			flight.ID, latest, string(data)); err != nil {
			return err
		}
		appended = true
	}
	if !appended {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM swim_radar_positions WHERE flight_id=? AND at_ns<=
(SELECT at_ns FROM swim_radar_positions WHERE flight_id=? ORDER BY at_ns DESC LIMIT 1 OFFSET ?)`,
		flight.ID, flight.ID, swimRadarMaxFixes); err != nil {
		return err
	}
	return tx.Commit()
}

// Retire the old scope even if no callback has arrived yet. Revisions also act
// as a floor when a scope later returns to its previous values.
func retireSWIMRadarScope(ctx context.Context, tx *sql.Tx, prior, flight feeds.Flight, previousProvider, provider string) error {
	if prior.ID == "" {
		return nil
	}
	if previousProvider == "" {
		previousProvider = prior.AeroProviderID()
	}
	if (provider == "" || provider == previousProvider) && prior.Source == flight.Source &&
		prior.OriginICAO == flight.OriginICAO && prior.DestinationICAO == flight.DestinationICAO &&
		strings.EqualFold(prior.TailNumber, flight.TailNumber) &&
		sameSWIMInstant(prior.ScheduledDeparture, flight.ScheduledDeparture) &&
		sameSWIMInstant(prior.ActualTakeoff, flight.ActualTakeoff) {
		return nil
	}
	priorScope := previousProvider != "" && prior.Source == "flightaware_aeroapi" && prior.OriginICAO != "" &&
		prior.DestinationICAO != "" && prior.ScheduledDeparture != nil && prior.ActualTakeoff != nil
	// A provider update can describe a change that precedes a fix we already
	// stored. Advance past the erased track as well as the provider timestamp.
	result, err := tx.ExecContext(ctx, `INSERT INTO swim_radar_revisions(flight_id,revision_ns)
SELECT ?,MAX(?,COALESCE((SELECT MAX(at_ns) FROM swim_radar_positions WHERE flight_id=?),0))
WHERE ? OR EXISTS(SELECT 1 FROM swim_radar_revisions WHERE flight_id=?)
ON CONFLICT(flight_id) DO UPDATE SET revision_ns=MAX(swim_radar_revisions.revision_ns+1,excluded.revision_ns)`,
		flight.ID, flight.ObservedAt.UnixNano(), flight.ID, priorScope, flight.ID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM swim_radar_positions WHERE flight_id=?`, flight.ID)
	return err
}

func sameSWIMInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

type swimRadarCursor struct {
	after *time.Time
	known *int64
}

func readSWIMRadarPositions(ctx context.Context, tx *sql.Tx, id string, cursor swimRadarCursor, limit int) ([]feeds.Position, *int64, error) {
	// The combined page has already checked tracking in this transaction.
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision_ns FROM swim_radar_revisions WHERE flight_id=?),0)`, id).Scan(&revision); err != nil {
		return nil, nil, err
	}
	if cursor.known != nil && *cursor.known != revision {
		cursor.after = nil
	}
	cutoff := time.Now().Add(-swimRadarRetention).UnixNano()
	positions, err := readRadarFixPage(ctx, tx, `SELECT p.data FROM swim_radar_positions p WHERE p.flight_id=? AND p.at_ns>?`,
		[]any{id, cutoff}, cursor.after, limit)
	return positions, &revision, err
}
