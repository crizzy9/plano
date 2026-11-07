package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const (
	checkInSchedulerInterval = 500 * time.Millisecond
	checkInSchedulerBatch    = 16
	checkInDepartureSQL      = `julianday(COALESCE(json_extract(snapshot,'$.estimatedDeparture'),json_extract(snapshot,'$.scheduledDeparture')))`
	// Match the predeparture check-in states in flightAlertRelevant.
	checkInPredepartureSQL = `json_extract(snapshot,'$.actualDeparture') IS NULL
  AND json_extract(snapshot,'$.actualTakeoff') IS NULL AND json_extract(snapshot,'$.actualLanding') IS NULL
  AND json_extract(snapshot,'$.actualArrival') IS NULL
  AND COALESCE(json_extract(snapshot,'$.status'),'') NOT IN ('taxi_out','airborne','landed','arrived','cancelled','diverted')`
	checkInPendingDeviceSQL = `EXISTS (
  SELECT 1 FROM tracked t JOIN devices d ON d.owner=t.owner AND d.kind='alert'
  WHERE t.flight_id=f.id AND NOT EXISTS (
    SELECT 1 FROM check_in_queued q WHERE q.device_id=d.id AND q.flight_id=f.id
  )
)`
	checkInScheduleSchema = `
CREATE INDEX IF NOT EXISTS flights_check_in_departure ON flights(` + checkInDepartureSQL + `,id) WHERE ` + checkInPredepartureSQL + `;
CREATE INDEX IF NOT EXISTS devices_check_in_owner ON devices(owner) WHERE kind='alert';`
	checkInCandidateSelectSQL = `SELECT f.id,` + checkInDepartureSQL + ` FROM flights f WHERE ` + checkInPredepartureSQL
	dueCheckInFlightsBaseSQL  = checkInCandidateSelectSQL + `
AND ` + checkInDepartureSQL + `>=julianday(?) AND ` + checkInDepartureSQL + `<=julianday(?)
AND ` + checkInPendingDeviceSQL
	dueCheckInFlightsSQL            = dueCheckInFlightsBaseSQL + ` ORDER BY ` + checkInDepartureSQL + `,f.id LIMIT ?`
	sameDepartureCheckInFlightsSQL  = dueCheckInFlightsBaseSQL + ` AND ` + checkInDepartureSQL + `=? AND f.id>? ORDER BY f.id LIMIT ?`
	laterDepartureCheckInFlightsSQL = checkInCandidateSelectSQL + ` AND ` + checkInDepartureSQL + `>? AND ` + checkInDepartureSQL + `<=julianday(?)
AND ` + checkInPendingDeviceSQL + ` ORDER BY ` + checkInDepartureSQL + `,f.id LIMIT ?`
)

type checkInCandidate struct {
	id           string
	departureDay float64
}

func checkInOpeningMovedLater(before, after feeds.Flight, now time.Time) bool {
	prior, next := effectiveDeparture(before), effectiveDeparture(after)
	return prior != nil && next != nil && next.After(*prior) && now.Before(next.Add(-24*time.Hour))
}

func (s *Store) runCheckInScheduler(ctx context.Context, logger *slog.Logger) {
	tick := time.NewTicker(checkInSchedulerInterval)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := s.queueDueCheckIns(ctx, time.Now()); err != nil && ctx.Err() == nil {
			logger.Error("check-in scheduler failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// The read can use cached snapshots from older database versions. An empty
// pass takes no write lock. Each candidate is rechecked under its own write
// transaction so tracking, flight state and outbox insertion stay atomic.
// Drain bounded pages without waiting for another scheduler tick. Advancing
// past every candidate lets later flights proceed when an earlier one skips
// or fails; the next ordinary scan can retry those flights.
func (s *Store) queueDueCheckIns(ctx context.Context, now time.Time) error {
	var after *checkInCandidate
	var firstError error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		candidates, err := s.dueCheckInFlights(ctx, now, after)
		if cancelled := ctx.Err(); cancelled != nil {
			return cancelled
		}
		if err != nil {
			return errors.Join(firstError, err)
		}
		for _, candidate := range candidates {
			if err := ctx.Err(); err != nil {
				return err
			}
			err := s.queueScheduledCheckIn(ctx, candidate.id, now)
			if cancelled := ctx.Err(); cancelled != nil {
				return cancelled
			}
			if err != nil && firstError == nil {
				firstError = fmt.Errorf("queue check-in for flight %s: %w", candidate.id, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(candidates) < checkInSchedulerBatch {
			return firstError
		}
		last := candidates[len(candidates)-1]
		after = &last
	}
}

func (s *Store) dueCheckInFlights(ctx context.Context, now time.Time, after *checkInCandidate) ([]checkInCandidate, error) {
	lower, upper := now.Format(time.RFC3339Nano), now.Add(24*time.Hour).Format(time.RFC3339Nano)
	if after == nil {
		return s.readCheckInCandidates(ctx, dueCheckInFlightsSQL, lower, upper, checkInSchedulerBatch)
	}
	// Separate the equal-key suffix from later keys so SQLite can seek both
	// expression-index columns without sorting or scanning the prior prefix.
	tied, err := s.readCheckInCandidates(ctx, sameDepartureCheckInFlightsSQL,
		lower, upper, after.departureDay, after.id, checkInSchedulerBatch)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(tied) == checkInSchedulerBatch {
		return tied, nil
	}
	// after came from this same fixed-time sweep. Its SQL key already meets
	// the lower window bound, so a greater key cannot fall below that bound.
	later, err := s.readCheckInCandidates(ctx, laterDepartureCheckInFlightsSQL,
		after.departureDay, upper, checkInSchedulerBatch-len(tied))
	if err != nil {
		return nil, err
	}
	return append(tied, later...), nil
}

func (s *Store) readCheckInCandidates(ctx context.Context, query string, args ...any) ([]checkInCandidate, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []checkInCandidate
	for rows.Next() {
		var candidate checkInCandidate
		if err := rows.Scan(&candidate.id, &candidate.departureDay); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *Store) queueScheduledCheckIn(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT f.snapshot FROM flights f WHERE f.id=? AND `+checkInPendingDeviceSQL, id).Scan(&raw)
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
	// SQLite rounds sub-millisecond timestamps. Check the exact opening
	// before emitting the event or waking the dispatcher.
	if checkInDeadline(flight, now) == nil {
		return nil
	}
	if err := queueCheckIn(ctx, tx, flight, now); err != nil {
		return err
	}
	return s.commitOutbox(tx)
}
