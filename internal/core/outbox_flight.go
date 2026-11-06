package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"

	"github.com/crizzy9/plano/internal/push"
)

const (
	boardingAlertLifetime   = 15 * time.Minute
	flightDelayAlertMinutes = 10
)

// Flight alerts keep their timeline event ID in the stored collapse field.
// Provider-specific coalescing is applied only after this join.
const outboxFlightEvent = `
LEFT JOIN events e ON e.id=json_extract(o.message,'$.CollapseID') AND e.flight_id=o.flight_id
  AND json_extract(o.message,'$.Kind')='alert'`

// A marker for a different lease cannot prove that a legacy worker left the
// current claim unsent. A never-attempted unleased row is also safe to reoffer.
const unsentCheckInLease = `(o.lease_until=0 AND COALESCE(l.may_have_sent,0)=0 OR l.lease_until=o.lease_until AND l.may_have_sent=0)`

func retireRemappedFlightJobs(ctx context.Context, tx *sql.Tx, flightID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM check_in_queued WHERE flight_id=?`, flightID); err != nil {
		return err
	}
	// Let a fresh start be queued when its previous job could not have sent.
	// An older lease without submission tracking or a retry is ambiguous.
	if _, err := tx.ExecContext(ctx, `DELETE FROM live_started WHERE flight_id=? AND device_id IN (
  SELECT o.device_id FROM outbox o LEFT JOIN outbox_live_start_leases l ON l.outbox_id=o.id
	  WHERE o.flight_id=? AND o.done=0 AND json_extract(o.message,'$.Kind')=?
  AND `+possibleLiveStart+`=0
)`, flightID, flightID, push.LiveStart); err != nil {
		return err
	}
	// The snapshot update invalidates connection assessments separately. Their
	// jobs wait for re-evaluation even if the remapped flight is the outbound.
	_, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE flight_id=? AND done=0
AND NOT EXISTS (SELECT 1 FROM connection_monitor_jobs WHERE outbox_id=outbox.id)`, flightID)
	return err
}

func pruneSupersededFlightAlerts(ctx context.Context, tx *sql.Tx, flightID string, kinds []string) error {
	if len(kinds) == 0 {
		return nil
	}
	if slices.Contains(kinds, "check_in") {
		// Reoffer an unsent reminder if check-in reopens. A legacy lease
		// without a submission marker could already have reached transport.
		if _, err := tx.ExecContext(ctx, `DELETE FROM check_in_queued AS c WHERE c.flight_id=? AND EXISTS (
  SELECT 1 FROM outbox o`+outboxFlightEvent+` LEFT JOIN outbox_check_in_leases l ON l.outbox_id=o.id
  WHERE o.flight_id=c.flight_id AND o.device_id=c.device_id AND o.done=0 AND o.attempts=0
  AND `+unsentCheckInLease+` AND json_extract(e.data,'$.kind')='check_in'
)`, flightID); err != nil {
			return err
		}
	}
	data, err := json.Marshal(kinds)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM outbox AS o WHERE o.flight_id=? AND o.done=0
AND json_extract(o.message,'$.Kind')='alert' AND EXISTS (
  SELECT 1 FROM events e WHERE e.id=json_extract(o.message,'$.CollapseID') AND e.flight_id=o.flight_id
  AND json_extract(e.data,'$.kind') IN (SELECT value FROM json_each(?))
)`, flightID, string(data))
	return err
}

// authorizeCheckIn records possible submission only if the exact lease is
// still current. This write and flight correction cannot pass each other.
func (s *Store) authorizeCheckIn(ctx context.Context, job deliveryJob, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO outbox_check_in_leases(outbox_id,lease_until,may_have_sent)
SELECT id,lease_until,1 FROM outbox WHERE id=? AND device_id=? AND lease_until=? AND lease_until>? AND done=0 AND expires>?
ON CONFLICT(outbox_id) DO UPDATE SET lease_until=excluded.lease_until,may_have_sent=1`,
		job.id, job.deviceID, job.lease, now.UnixNano(), now.UnixNano())
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	return updated == 1, err
}
