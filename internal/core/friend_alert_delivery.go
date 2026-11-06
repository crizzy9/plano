package core

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/crizzy9/plano/internal/push"
)

const eligibleFriendAlertGrants = `SELECT r.viewer,g.friend_id,g.flight_id
FROM friend_flights g JOIN friend_relationships r ON r.id=g.friend_id
JOIN friend_alert_preferences p ON p.friend_id=g.friend_id
WHERE g.flight_id=?1 AND (p.mode='all' OR (p.mode='arrival' AND ?2))`

// A friend job keeps its marker even when its original grants are revoked.
// Recreating a grant cannot authorize a job queued before the revocation.
const revokedFriendAlert = `EXISTS (
  SELECT 1 FROM outbox_friend_jobs j WHERE j.outbox_id=o.id
  AND NOT EXISTS(SELECT 1 FROM outbox_friend_grants g WHERE g.outbox_id=j.outbox_id)
)`

func enqueueFriendFlightAlert(ctx context.Context, tx *sql.Tx, flightID string, msg push.Message, event Event) error {
	if event.Kind == "check_in" {
		return nil
	}
	arrival := event.Kind == "landed" || event.Kind == "arrived"
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	jobs, err := tx.QueryContext(ctx, `WITH eligible AS (`+eligibleFriendAlertGrants+`)
INSERT INTO outbox(device_id,flight_id,message,due,expires)
SELECT d.id,?1,?3,?4,?5 FROM devices d
WHERE d.kind='alert' AND d.owner IN (SELECT viewer FROM eligible)
AND NOT EXISTS(SELECT 1 FROM tracked t WHERE t.owner=d.owner AND t.flight_id=?1)
RETURNING id`, flightID, arrival, string(raw), msg.Timestamp.UnixNano(), msg.ExpiresAt.UnixNano())
	if err != nil {
		return err
	}
	jobIDs, err := readOutboxJobIDs(jobs)
	if err != nil || len(jobIDs) == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_friend_jobs(outbox_id) SELECT value FROM json_each(?)`, string(jobIDs)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `WITH eligible AS (`+eligibleFriendAlertGrants+`)
INSERT INTO outbox_friend_grants(outbox_id,friend_id,flight_id)
SELECT o.id,g.friend_id,g.flight_id FROM json_each(?3) j JOIN outbox o ON o.id=j.value
JOIN devices d ON d.id=o.device_id JOIN eligible g ON g.viewer=d.owner`, flightID, arrival, string(jobIDs)); err != nil {
		return err
	}
	if len(event.advisoryEvidence) != 0 {
		return linkOutboxAdvisories(ctx, tx, jobIDs, event.advisoryEvidence)
	}
	return nil
}
