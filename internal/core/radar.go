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
CREATE TABLE IF NOT EXISTS radar_receiver_revisions (
  flight_id TEXT PRIMARY KEY REFERENCES flights(id) ON DELETE CASCADE,
  revision_ns INTEGER NOT NULL, last_tail TEXT NOT NULL
);
`

const radarFlightStatus = `json_extract(snapshot,'$.status') IN ('airborne','diverted')
AND json_extract(snapshot,'$.actualTakeoff') IS NOT NULL
AND json_extract(snapshot,'$.actualLanding') IS NULL
AND json_extract(snapshot,'$.actualArrival') IS NULL`

const radarFlightCandidates = `FROM flights WHERE ` + radarFlightStatus + `
AND observed_ns>=?
AND (COALESCE(json_extract(snapshot,'$.estimatedArrival'),json_extract(snapshot,'$.scheduledArrival')) IS NULL
 OR julianday(COALESCE(json_extract(snapshot,'$.estimatedArrival'),json_extract(snapshot,'$.scheduledArrival')))>=julianday(?))
AND EXISTS (SELECT 1 FROM tracked WHERE flight_id=flights.id)`

type radarSample struct {
	flight   feeds.Flight
	position feeds.Position
}

type RadarPage struct {
	Positions         []feeds.Position
	ReceiverRevision  *int64
	ProviderPositions []feeds.Position
	ProviderRevision  *time.Time
}

func resetRadarReceiver(ctx context.Context, tx *sql.Tx, flight feeds.Flight, lastTail string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM radar_positions WHERE flight_id=?`, flight.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO radar_receiver_revisions(flight_id,revision_ns,last_tail) VALUES(?,?,?)
ON CONFLICT(flight_id) DO UPDATE SET revision_ns=excluded.revision_ns,last_tail=excluded.last_tail`,
		flight.ID, flight.ObservedAt.UnixNano(), lastTail)
	return err
}

func recordRadarSamples(ctx context.Context, tx *sql.Tx, samples []radarSample) error {
	for _, sample := range samples {
		if sample.flight.ActualTakeoff == nil || sample.position.At.Before(*sample.flight.ActualTakeoff) {
			continue
		}
		data, err := json.Marshal(sample.position)
		if err != nil {
			return err
		}
		// Apply the observation only to the selected flight snapshot.
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
	return nil
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
	now := time.Now()
	var active bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 `+radarFlightCandidates+`)`,
		now.Add(-15*time.Minute).UnixNano(), now.Add(-30*time.Minute).Format(time.RFC3339)).Scan(&active)
	if err != nil || !active {
		return err
	}
	snapshot, err := receiver.Snapshot(ctx)
	if err != nil {
		return err
	}
	// Recheck the eligible legs after the network request. Keep matching and
	// writing in one transaction so a concurrent refresh cannot add a conflict.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now = time.Now()
	rows, err := tx.QueryContext(ctx, `SELECT snapshot `+radarFlightCandidates,
		now.Add(-15*time.Minute).UnixNano(), now.Add(-30*time.Minute).Format(time.RFC3339))
	if err != nil {
		return err
	}
	defer rows.Close()
	matches := make(map[string]radarSample)
	for rows.Next() {
		var raw []byte
		var flight feeds.Flight
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &flight); err != nil {
			return err
		}
		var modeS string
		if registry != nil {
			modeS = registry.ModeSHex(flight.TailNumber)
		}
		position, address, ok := snapshot.Match(flight, modeS)
		if !ok || flight.ActualTakeoff == nil || position.At.Before(*flight.ActualTakeoff) {
			continue
		}
		if _, exists := matches[address]; exists {
			// Keep the address marked ambiguous even if more legs match it.
			matches[address] = radarSample{}
		} else {
			matches[address] = radarSample{flight, position}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var samples []radarSample
	for _, sample := range matches {
		if sample.flight.ID != "" {
			samples = append(samples, sample)
		}
	}
	if err := recordRadarSamples(ctx, tx, samples); err != nil {
		return err
	}
	return tx.Commit()
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
	return readRadarPositions(ctx, s.db, owner, id, after, nil, limit)
}

type radarRowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readRadarPositions(ctx context.Context, db radarRowsQueryer, owner, id string, after *time.Time, observedNS *int64, limit int) ([]feeds.Position, error) {
	query := `SELECT p.data FROM radar_positions p JOIN flights f ON f.id=p.flight_id
WHERE p.flight_id=? AND EXISTS(SELECT 1 FROM tracked WHERE owner=? AND flight_id=p.flight_id)
AND COALESCE(json_extract(f.snapshot,'$.tailNumber'),'')<>''`
	args := []any{id, owner}
	if observedNS != nil {
		query += ` AND f.observed_ns=?`
		args = append(args, *observedNS)
	}
	if after == nil {
		query += ` ORDER BY p.at_ns DESC LIMIT ?`
	} else {
		query += ` AND p.at_ns>? ORDER BY p.at_ns ASC LIMIT ?`
		args = append(args, after.UnixNano())
	}
	rows, err := db.QueryContext(ctx, query, append(args, limit)...)
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

// RadarProviderPositions pages cached provider positions independently of the
// local receiver. A known revision turns the read into a snapshot refresh.
func (s *Store) RadarProviderPositions(ctx context.Context, owner, id string, after, knownRevision *time.Time, limit int) ([]feeds.Position, *time.Time, error) {
	if limit < 1 || limit > 2000 {
		return nil, nil, errors.New("invalid radar page size")
	}
	return readRadarProviderPositions(ctx, s.db, owner, id, after, knownRevision, limit)
}

func readRadarProviderPositions(ctx context.Context, db rowQueryer, owner, id string, after, knownRevision *time.Time, limit int) ([]feeds.Position, *time.Time, error) {
	var knownNS any
	if knownRevision != nil {
		knownNS = knownRevision.UnixNano()
	}
	var telemetry []byte
	var revision sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT
  CASE WHEN t.owner IS NOT NULL THEN f.observed_ns END,
  CASE WHEN t.owner IS NOT NULL AND f.observed_ns IS NOT ? THEN f.telemetry END
FROM flights f LEFT JOIN tracked t ON t.owner=? AND t.flight_id=f.id
WHERE f.id=?`, knownNS, owner, id).Scan(&revision, &telemetry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if !revision.Valid {
		return []feeds.Position{}, nil, nil
	}
	currentRevision := time.Unix(0, revision.Int64).UTC()
	if len(telemetry) == 0 {
		return []feeds.Position{}, &currentRevision, nil
	}
	if knownRevision != nil {
		// A replacement snapshot can revise fixes behind the previous cursor.
		after = nil
	}
	var positions []feeds.Position
	if err := json.Unmarshal(telemetry, &positions); err != nil {
		return nil, nil, err
	}
	latest := time.Now().Add(30 * time.Second)
	positions = slices.DeleteFunc(positions, func(p feeds.Position) bool {
		year := p.At.UTC().Year()
		return p.Source == "operator_readsb" || year < 2000 || year > 2100 || p.At.After(latest) || after != nil && !p.At.After(*after)
	})
	if len(positions) == 0 {
		return []feeds.Position{}, &currentRevision, nil
	}
	// Keep the first cached fix per instant so a timestamp-only cursor never
	// splits simultaneous positions between pages.
	slices.SortStableFunc(positions, func(a, b feeds.Position) int { return a.At.Compare(b.At) })
	positions = slices.CompactFunc(positions, func(a, b feeds.Position) bool { return a.At.Equal(b.At) })
	if len(positions) > limit {
		if after == nil {
			positions = positions[len(positions)-limit:]
		} else {
			positions = positions[:limit]
		}
	}
	return positions, &currentRevision, nil
}

// RadarPage reads both feeds from one snapshot so a receiver reset cannot
// pair fixes from the previous aircraft with a newer provider revision.
func (s *Store) RadarPage(ctx context.Context, owner, id string, receiverAfter *time.Time, receiverKnown *int64, providerAfter, providerKnown *time.Time, receiverLimit, providerLimit int) (RadarPage, error) {
	if receiverLimit < 1 || receiverLimit > 2000 || providerLimit < 1 || providerLimit > 2000 {
		return RadarPage{}, errors.New("invalid radar page size")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RadarPage{}, err
	}
	defer tx.Rollback()
	var revision sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN t.owner IS NOT NULL THEN COALESCE(r.revision_ns,0) END
FROM flights f LEFT JOIN tracked t ON t.owner=? AND t.flight_id=f.id
LEFT JOIN radar_receiver_revisions r ON r.flight_id=f.id WHERE f.id=?`, owner, id).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return RadarPage{}, ErrNotFound
	}
	if err != nil {
		return RadarPage{}, err
	}
	if !revision.Valid {
		return RadarPage{Positions: []feeds.Position{}, ProviderPositions: []feeds.Position{}}, nil
	}
	page := RadarPage{ReceiverRevision: &revision.Int64}
	if receiverKnown != nil && *receiverKnown != revision.Int64 {
		receiverAfter = nil
	}
	page.Positions, err = readRadarPositions(ctx, tx, owner, id, receiverAfter, nil, receiverLimit)
	if err != nil {
		return RadarPage{}, err
	}
	page.ProviderPositions, page.ProviderRevision, err = readRadarProviderPositions(ctx, tx, owner, id, providerAfter, providerKnown, providerLimit)
	if err != nil {
		return RadarPage{}, err
	}
	return page, tx.Commit()
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
