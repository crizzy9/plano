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
	dueCheckInFlightsSQL = `SELECT f.id FROM flights f WHERE ` + checkInPredepartureSQL + `
AND ` + checkInDepartureSQL + `>=julianday(?) AND ` + checkInDepartureSQL + `<=julianday(?)
AND ` + checkInPendingDeviceSQL + ` ORDER BY ` + checkInDepartureSQL + `,f.id LIMIT ?`
)

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
func (s *Store) queueDueCheckIns(ctx context.Context, now time.Time) error {
	ids, err := s.dueCheckInFlights(ctx, now)
	if err != nil {
		return err
	}
	var firstError error
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.queueScheduledCheckIn(ctx, id, now); err != nil && firstError == nil {
			firstError = fmt.Errorf("queue check-in for flight %s: %w", id, err)
		}
	}
	return firstError
}

func (s *Store) dueCheckInFlights(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, dueCheckInFlightsSQL, now.Format(time.RFC3339Nano),
		now.Add(24*time.Hour).Format(time.RFC3339Nano), checkInSchedulerBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
