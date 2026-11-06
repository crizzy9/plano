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

func retireRemappedFlightJobs(ctx context.Context, tx *sql.Tx, flightID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM check_in_queued WHERE flight_id=?`, flightID); err != nil {
		return err
	}
	// Let a fresh start be queued when its previous job could not have sent.
	// An older lease without submission tracking or a retry is ambiguous.
	if _, err := tx.ExecContext(ctx, `DELETE FROM live_started WHERE flight_id=? AND device_id IN (
  SELECT o.device_id FROM outbox o LEFT JOIN outbox_live_start_leases l ON l.outbox_id=o.id
  WHERE o.flight_id=? AND o.done=0 AND o.attempts=0 AND json_extract(o.message,'$.Kind')=?
  AND COALESCE(l.may_have_sent,o.lease_until<>0)=0
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
		// An unclaimed reminder can be offered again if check-in reopens.
		// Keep the latch once dispatch could have submitted it to a provider.
		if _, err := tx.ExecContext(ctx, `DELETE FROM check_in_queued AS c WHERE c.flight_id=? AND EXISTS (
  SELECT 1 FROM outbox o`+outboxFlightEvent+` WHERE o.flight_id=c.flight_id AND o.device_id=c.device_id AND o.done=0
  AND o.lease_until=0 AND o.attempts=0 AND json_extract(e.data,'$.kind')='check_in'
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
