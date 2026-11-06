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

// A Friends job keeps its marker after either original authorization is removed.
// A later grant or tracking row does not authorize an older job.
const revokedFriendAlert = `EXISTS (
  SELECT 1 FROM outbox_friend_jobs j WHERE j.outbox_id=o.id
  AND NOT EXISTS(SELECT 1 FROM outbox_friend_grants g WHERE g.outbox_id=j.outbox_id)
  AND NOT EXISTS(SELECT 1 FROM outbox_friend_personal p WHERE p.outbox_id=j.outbox_id)
)`

func enqueueFlightAlert(ctx context.Context, tx *sql.Tx, flightID string, msg push.Message, event Event) error {
	if event.Kind == "check_in" || event.inboundLegID != "" {
		return enqueueWithAdvisories(ctx, tx, flightID, msg, event.advisoryEvidence,
			`SELECT d.id FROM devices d JOIN tracked t ON t.owner=d.owner WHERE t.flight_id=? AND d.kind='alert'`, flightID)
	}
	arrival := event.Kind == "landed" || event.Kind == "arrived"
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	jobs, err := tx.QueryContext(ctx, `WITH eligible AS (`+eligibleFriendAlertGrants+`), audience AS (
  SELECT owner FROM tracked WHERE flight_id=?1 UNION SELECT viewer FROM eligible
)
INSERT INTO outbox(device_id,flight_id,message,due,expires)
SELECT d.id,?1,?3,?4,?5 FROM audience a JOIN devices d ON d.owner=a.owner WHERE d.kind='alert'
RETURNING id`, flightID, arrival, string(raw), msg.Timestamp.UnixNano(), msg.ExpiresAt.UnixNano())
	if err != nil {
		return err
	}
	jobIDs, err := readOutboxJobIDs(jobs)
	if err != nil || len(jobIDs) == 0 {
		return err
	}
	if err := linkFlightAlertGrants(ctx, tx, flightID, arrival, string(jobIDs)); err != nil {
		return err
	}
	if len(event.advisoryEvidence) != 0 {
		return linkOutboxAdvisories(ctx, tx, jobIDs, event.advisoryEvidence)
	}
	return nil
}

func linkFlightAlertGrants(ctx context.Context, tx *sql.Tx, flightID string, arrival bool, jobIDs string) error {
	marked, err := tx.ExecContext(ctx, `WITH eligible AS (`+eligibleFriendAlertGrants+`)
INSERT INTO outbox_friend_jobs(outbox_id)
SELECT o.id FROM json_each(?3) ids JOIN outbox o ON o.id=ids.value JOIN devices d ON d.id=o.device_id
WHERE d.owner IN (SELECT viewer FROM eligible)`, flightID, arrival, jobIDs)
	if err != nil {
		return err
	}
	if count, err := marked.RowsAffected(); err != nil || count == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `WITH eligible AS (`+eligibleFriendAlertGrants+`)
INSERT INTO outbox_friend_grants(outbox_id,friend_id,flight_id)
SELECT o.id,g.friend_id,g.flight_id FROM json_each(?3) j JOIN outbox o ON o.id=j.value
JOIN devices d ON d.id=o.device_id JOIN eligible g ON g.viewer=d.owner`, flightID, arrival, jobIDs); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox_friend_personal(outbox_id,owner,flight_id)
SELECT o.id,t.owner,t.flight_id FROM json_each(?) ids JOIN outbox_friend_jobs j ON j.outbox_id=ids.value
JOIN outbox o ON o.id=j.outbox_id JOIN devices d ON d.id=o.device_id
JOIN tracked t ON t.owner=d.owner AND t.flight_id=o.flight_id`, jobIDs)
	return err
}
