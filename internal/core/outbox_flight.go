package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"
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

func pruneSupersededFlightAlerts(ctx context.Context, tx *sql.Tx, flightID string, kinds []string) error {
	if len(kinds) == 0 {
		return nil
	}
	if slices.Contains(kinds, "check_in") {
		// An unclaimed reminder can be offered again if check-in reopens.
		// Keep the latch once dispatch could have submitted it to a provider.
		if _, err := tx.ExecContext(ctx, `DELETE FROM check_in_queued AS c WHERE c.flight_id=? AND EXISTS (
  SELECT 1 FROM outbox o WHERE o.flight_id=c.flight_id AND o.device_id=c.device_id AND o.done=0
  AND o.lease_until=0 AND o.attempts=0 AND json_extract(o.message,'$.CollapseID')=?
)`, flightID, hash(flightID+"\x00check_in")); err != nil {
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
