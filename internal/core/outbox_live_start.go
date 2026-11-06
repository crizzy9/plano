package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

// authorizeLiveStart checks and marks the lease in the same transaction as
// the current flight read. Later observations can then distinguish an unsent
// start from one that could already be at the provider.
func (s *Store) authorizeLiveStart(ctx context.Context, job *deliveryJob, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var snapshot []byte
	var seat string
	var unsent, localActivity bool
	err = tx.QueryRowContext(ctx, `SELECT f.snapshot,t.seat,
o.attempts=0 AND COALESCE(l.may_have_sent,1)=0,
d.platform='ios' AND EXISTS (
  SELECT 1 FROM devices u LEFT JOIN device_bindings ub ON ub.device_id=u.id
  WHERE u.owner=d.owner AND u.kind='live_update' AND u.flight_id=o.flight_id AND COALESCE(ub.binding,'')=?
)
FROM outbox o JOIN devices d ON d.id=o.device_id JOIN flights f ON f.id=o.flight_id
JOIN tracked t ON t.owner=d.owner AND t.flight_id=o.flight_id
LEFT JOIN outbox_live_start_leases l ON l.outbox_id=o.id
WHERE o.id=? AND o.device_id=? AND o.lease_until=? AND o.done=0`,
		job.target.Binding, job.id, job.deviceID, job.lease).Scan(&snapshot, &seat, &unsent, &localActivity)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var flight feeds.Flight
	if err := json.Unmarshal(snapshot, &flight); err != nil {
		return false, err
	}
	if localActivity || finished(flight) || !inLiveStartWindow(flight, now) {
		// If an earlier attempt might have reached the provider, retain its
		// latch. An existing local iOS activity also needs no remote start.
		if unsent && !localActivity {
			if err := retireUnsentLiveStart(ctx, tx, *job); err != nil {
				return false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id=?`, job.id); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}

	message := job.message
	// Android uses the start's original observation to fence queued starts
	// after a device-local Stop. Newer Android state follows as a live update.
	if unsent && job.target.Platform == "ios" {
		latest := liveMessage(flight, push.LiveStart, seat, now)
		if message.State == nil || *message.State != *latest.State || message.LiveObservedNs != latest.LiveObservedNs {
			// Keep the original wire timestamp, expiry and registration
			// options. Persist the refreshed content for any later retry.
			message.State, message.LiveObservedNs = latest.State, latest.LiveObservedNs
			message.StaleAt, message.Title = latest.StaleAt, latest.Title
			raw, err := json.Marshal(message)
			if err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE outbox SET message=? WHERE id=?`, string(raw), job.id); err != nil {
				return false, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_live_start_leases VALUES(?,1)
ON CONFLICT(outbox_id) DO UPDATE SET may_have_sent=1`, job.id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	job.message = message
	return true, nil
}

func retireUnsentLiveStart(ctx context.Context, tx *sql.Tx, job deliveryJob) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM live_started WHERE device_id=? AND flight_id=?`,
		job.deviceID, job.message.FlightID); err != nil {
		return err
	}
	if job.target.Platform != "android" {
		return nil
	}
	// An update can create Android progress when a start is missed. Remove
	// followers that assumed this never-submitted start would reach the device.
	_, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE device_id=? AND flight_id=? AND id>? AND done=0
AND json_extract(message,'$.Kind') IN ('update','end')`, job.deviceID, job.message.FlightID, job.id)
	return err
}
