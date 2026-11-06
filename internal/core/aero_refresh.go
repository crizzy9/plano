package core

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

// An older database may have tracked exact AeroAPI IDs with no alias row yet.
// A later source observation records aliases for schedule-to-live transitions.
const aeroTargetsSQL = `SELECT f.id FROM flights f WHERE EXISTS(SELECT 1 FROM tracked t WHERE t.flight_id=f.id)
AND (f.id IN (SELECT flight_id FROM aero_instance_keys WHERE provider_id=?) OR
(f.id=? AND json_extract(f.snapshot,'$.source')='flightaware_aeroapi' AND NOT EXISTS(SELECT 1 FROM aero_instance_keys WHERE flight_id=f.id)))`

// Only an explicit direct link from a tracked, predeparture flight authorizes
// an inbound callback. A cached first leg must agree with that link.
const aeroInboundTargetsSQL = `SELECT f.id FROM flights f
WHERE json_extract(f.snapshot,'$.inboundId')=?
AND json_extract(f.snapshot,'$.source') IN ('flightaware_aeroapi','flightaware_schedule')
AND julianday(json_extract(f.snapshot,'$.scheduledDeparture')) BETWEEN julianday(?) AND julianday(?)
AND json_extract(f.snapshot,'$.inboundId')<>f.id
AND NOT EXISTS(SELECT 1 FROM aero_instance_keys k WHERE k.flight_id=f.id AND k.provider_id=json_extract(f.snapshot,'$.inboundId'))
AND COALESCE(json_extract(f.inbound,'$[0].id'),'') IN ('',json_extract(f.snapshot,'$.inboundId'))
AND json_extract(f.snapshot,'$.status') NOT IN ('cancelled','taxi_out','airborne','landed','arrived','diverted')
AND json_extract(f.snapshot,'$.actualDeparture') IS NULL AND json_extract(f.snapshot,'$.actualTakeoff') IS NULL
AND json_extract(f.snapshot,'$.actualLanding') IS NULL AND json_extract(f.snapshot,'$.actualArrival') IS NULL
AND EXISTS(SELECT 1 FROM tracked t WHERE t.flight_id=f.id)`

func aeroInboundTargetArgs(providerID string, now time.Time) []any {
	return []any{providerID, now.Add(-6 * time.Hour).Format(time.RFC3339Nano), now.Add(25 * time.Hour).Format(time.RFC3339Nano)}
}

// QueueAeroRefresh persists an authenticated hint for a tracked flight or its
// direct inbound. A hint received while a fetch is leased advances its generation.
func (s *Store) QueueAeroRefresh(ctx context.Context, providerID string, now time.Time) error {
	args := append([]any{providerID, now.UnixNano(), providerID, providerID}, aeroInboundTargetArgs(providerID, now)...)
	result, err := s.db.ExecContext(ctx, `INSERT INTO aero_refresh_jobs(provider_id,generation,due_ns)
SELECT ?,1,? WHERE EXISTS(`+aeroTargetsSQL+`) OR EXISTS(`+aeroInboundTargetsSQL+`)
ON CONFLICT(provider_id) DO UPDATE SET generation=aero_refresh_jobs.generation+1,
due_ns=CASE WHEN aero_refresh_jobs.lease_ns=0 THEN MIN(aero_refresh_jobs.due_ns,excluded.due_ns) ELSE aero_refresh_jobs.due_ns END`, args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 0 {
		select {
		case s.refreshWake <- struct{}{}:
		default:
		}
	}
	return nil
}

type aeroRefreshJob struct {
	providerID        string
	generation, lease int64
	failures          int
}

func (s *Store) claimAeroRefresh(ctx context.Context, now time.Time) (*aeroRefreshJob, error) {
	var job aeroRefreshJob
	job.lease = now.Add(25 * time.Second).UnixNano()
	err := s.db.QueryRowContext(ctx, `UPDATE aero_refresh_jobs SET lease_ns=? WHERE provider_id=(
SELECT provider_id FROM aero_refresh_jobs WHERE due_ns<=? AND lease_ns<=? ORDER BY due_ns,provider_id LIMIT 1)
RETURNING provider_id,generation,failures`, job.lease, now.UnixNano(), now.UnixNano()).Scan(&job.providerID, &job.generation, &job.failures)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &job, err
}

func (s *Store) completeAeroRefresh(ctx context.Context, job *aeroRefreshJob, now time.Time, failed bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !failed || job.failures >= 6 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM aero_refresh_jobs WHERE provider_id=? AND lease_ns=? AND generation=?`, job.providerID, job.lease, job.generation); err != nil {
			return err
		}
	}
	due, failures := now, 0
	if failed {
		due, failures = now.Add(time.Second<<min(job.failures, 6)), job.failures+1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE aero_refresh_jobs SET lease_ns=0,
  due_ns=CASE WHEN generation=? THEN ? ELSE ? END,
  failures=CASE WHEN generation=? THEN ? ELSE 0 END
WHERE provider_id=? AND lease_ns=?`, job.generation, due.UnixNano(), now.UnixNano(), job.generation, failures, job.providerID, job.lease); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) refreshAeroFlight(ctx context.Context, source feeds.FlightSource, providerID string) error {
	args := append([]any{providerID, providerID}, aeroInboundTargetArgs(providerID, time.Now())...)
	rows, err := s.db.QueryContext(ctx, `SELECT id,0 FROM (`+aeroTargetsSQL+`)
UNION ALL SELECT '',1 WHERE EXISTS(`+aeroInboundTargetsSQL+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	var hasInboundTargets bool
	for rows.Next() {
		var id string
		var inbound bool
		if err := rows.Scan(&id, &inbound); err != nil {
			return err
		}
		if inbound {
			hasInboundTargets = true
		} else {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil || len(ids) == 0 && !hasInboundTargets {
		return err
	}
	flight, err := source.Get(ctx, providerID)
	if err != nil {
		return err
	}
	if flight.AeroProviderID() != providerID {
		return feeds.ErrUnavailable
	}
	for _, id := range ids {
		direct := flight
		direct.ID = id
		if err := s.observe(ctx, direct, nil, nil, nil, false, providerID); err != nil {
			return err
		}
	}
	if hasInboundTargets {
		return s.applyAeroInbound(ctx, providerID, flight, time.Now())
	}
	return nil
}

// RunAeroRefresh claims at most four provider fetches at once. Durable hints
// also resume after restart or after a process-local signal is lost.
func (s *Store) RunAeroRefresh(ctx context.Context, source feeds.FlightSource, logger *slog.Logger) {
	const slots = 4
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	completed := make(chan struct{}, slots)
	var workers sync.WaitGroup
	defer workers.Wait()
	busy := 0
	for ctx.Err() == nil {
		for busy < slots {
			job, err := s.claimAeroRefresh(ctx, time.Now())
			if err != nil && ctx.Err() == nil {
				logger.Error("could not claim AeroAPI refresh", "error", err)
			}
			if err != nil || job == nil {
				break
			}
			busy++
			workers.Go(func() {
				defer func() { completed <- struct{}{} }()
				attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
				err := s.refreshAeroFlight(attempt, source, job.providerID)
				cancel()
				if err != nil && ctx.Err() == nil {
					logger.Warn("AeroAPI callback refresh failed", "error", err)
				}
				if err := s.completeAeroRefresh(ctx, job, time.Now(), err != nil); err != nil && ctx.Err() == nil {
					logger.Error("could not resolve AeroAPI refresh", "error", err)
				}
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-completed:
			busy--
		case <-s.refreshWake:
		case <-tick.C:
		}
	}
}
