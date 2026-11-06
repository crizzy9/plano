package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const radarSchema = `
CREATE TABLE IF NOT EXISTS radar_positions (
  flight_id TEXT NOT NULL REFERENCES flights(id) ON DELETE CASCADE,
  at_ns INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(flight_id,at_ns)
);
CREATE INDEX IF NOT EXISTS radar_positions_expiry ON radar_positions(at_ns);
`

const radarFlightStatus = `json_extract(snapshot,'$.status') IN ('airborne','diverted')
AND json_extract(snapshot,'$.actualTakeoff') IS NOT NULL
AND json_extract(snapshot,'$.actualLanding') IS NULL
AND json_extract(snapshot,'$.actualArrival') IS NULL`

type radarSample struct {
	flight   feeds.Flight
	position feeds.Position
}

func (s *Store) radarFlights(ctx context.Context, now time.Time) ([]feeds.Flight, error) {
	return s.list(ctx, `SELECT snapshot FROM flights WHERE `+radarFlightStatus+`
AND observed_ns>=?
AND (COALESCE(json_extract(snapshot,'$.estimatedArrival'),json_extract(snapshot,'$.scheduledArrival')) IS NULL
 OR julianday(COALESCE(json_extract(snapshot,'$.estimatedArrival'),json_extract(snapshot,'$.scheduledArrival')))>=julianday(?))
AND EXISTS (SELECT 1 FROM tracked WHERE flight_id=flights.id)`, now.Add(-15*time.Minute).UnixNano(), now.Add(-30*time.Minute).Format(time.RFC3339))
}

func (s *Store) recordRadarSamples(ctx context.Context, samples []radarSample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sample := range samples {
		if sample.flight.ActualTakeoff == nil || sample.position.At.Before(*sample.flight.ActualTakeoff) {
			continue
		}
		data, err := json.Marshal(sample.position)
		if err != nil {
			return err
		}
		// A provider refresh can change aircraft or end the leg while the
		// receiver request is in flight. Apply only to the still-current leg.
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO radar_positions(flight_id,at_ns,data)
SELECT id,?,? FROM flights WHERE id=? AND observed_ns=? AND `+radarFlightStatus+`
AND json_extract(snapshot,'$.tailNumber')=?
AND COALESCE(json_extract(snapshot,'$.atcIdent'),'')=?
AND COALESCE(json_extract(snapshot,'$.icaoNumber'),'')=?
AND julianday(json_extract(snapshot,'$.actualTakeoff')) <= julianday(?)
AND NOT EXISTS(SELECT 1 FROM radar_positions WHERE flight_id=flights.id AND at_ns>=?)
AND EXISTS(SELECT 1 FROM tracked WHERE flight_id=flights.id)`,
			sample.position.At.UnixNano(), string(data), sample.flight.ID, sample.flight.ObservedAt.UnixNano(), sample.flight.TailNumber, sample.flight.ATCIdent,
			sample.flight.ICAONumber, sample.position.At.Format(time.RFC3339Nano), sample.position.At.UnixNano())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RunRadar samples one operator receiver for all airborne tracked flights.
// Source failures leave the existing history and status observation untouched.
func (s *Store) RunRadar(ctx context.Context, receiver *feeds.ADSBReceiver, registry *feeds.FAARegistry, cadence time.Duration, logger *slog.Logger) {
	tick := time.NewTicker(cadence)
	defer tick.Stop()
	for {
		if err := s.sampleRadar(ctx, receiver, registry); err != nil && ctx.Err() == nil {
			logger.Warn("radar sample unavailable", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Store) sampleRadar(ctx context.Context, receiver *feeds.ADSBReceiver, registry *feeds.FAARegistry) error {
	flights, err := s.radarFlights(ctx, time.Now())
	if err != nil || len(flights) == 0 {
		return err
	}
	snapshot, err := receiver.Snapshot(ctx)
	if err != nil {
		return err
	}
	var samples []radarSample
	for _, flight := range flights {
		var modeS string
		if registry != nil {
			modeS = registry.ModeSHex(flight.TailNumber)
		}
		if position, ok := snapshot.Match(flight, modeS); ok {
			samples = append(samples, radarSample{flight, position})
		}
	}
	return s.recordRadarSamples(ctx, samples)
}

// RadarPositions returns local receiver observations in chronological order.
// With no cursor, it returns the newest limit entries. A cursor pages forward.
func (s *Store) RadarPositions(ctx context.Context, owner, id string, after *time.Time, limit int) ([]feeds.Position, error) {
	if limit < 1 || limit > 2000 {
		return nil, errors.New("invalid radar page size")
	}
	if _, err := s.Flight(ctx, id); err != nil {
		return nil, err
	}
	return s.radarPositions(ctx, owner, id, after, limit)
}

func (s *Store) radarPositions(ctx context.Context, owner, id string, after *time.Time, limit int) ([]feeds.Position, error) {
	var rows *sql.Rows
	var err error
	if after == nil {
		rows, err = s.db.QueryContext(ctx, `SELECT data FROM radar_positions WHERE flight_id=? AND EXISTS(SELECT 1 FROM tracked WHERE owner=? AND flight_id=radar_positions.flight_id) ORDER BY at_ns DESC LIMIT ?`, id, owner, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT data FROM radar_positions WHERE flight_id=? AND at_ns>? AND EXISTS(SELECT 1 FROM tracked WHERE owner=? AND flight_id=radar_positions.flight_id) ORDER BY at_ns ASC LIMIT ?`, id, after.UnixNano(), owner, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	positions := make([]feeds.Position, 0)
	for rows.Next() {
		var raw []byte
		var p feeds.Position
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	if after == nil {
		slices.Reverse(positions)
	}
	return positions, rows.Err()
}

func mergeFlightPositions(provider, local []feeds.Position) []feeds.Position {
	if len(local) == 0 {
		return provider
	}
	all := append(provider, local...)
	slices.SortStableFunc(all, func(a, b feeds.Position) int { return a.At.Compare(b.At) })
	all = slices.CompactFunc(all, func(a, b feeds.Position) bool {
		return a.At.Equal(b.At) && a.Latitude == b.Latitude && a.Longitude == b.Longitude
	})
	if len(all) > 2000 {
		all = all[len(all)-2000:]
	}
	return all
}
