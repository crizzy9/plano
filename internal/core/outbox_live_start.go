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

// Older outbox rows lack submission markers. A prior lease or retry means
// their transport outcome cannot be reconstructed.
const possibleLiveStart = `COALESCE(l.may_have_sent,o.lease_until<>0 OR o.attempts<>0)`

const savedIOSLiveStarts = `SELECT d.id,d.owner,s.flight_id,COALESCE(b.binding,'') AS binding
FROM live_started s JOIN devices d ON d.id=s.device_id LEFT JOIN device_bindings b ON b.device_id=d.id
WHERE d.platform='ios' AND (EXISTS(SELECT 1 FROM live_start_confirmations c
WHERE c.device_id=d.id AND c.flight_id=s.flight_id) OR EXISTS(
SELECT 1 FROM outbox o LEFT JOIN outbox_live_start_leases l ON l.outbox_id=o.id
WHERE o.id=(SELECT latest.id FROM outbox latest WHERE latest.device_id=d.id AND latest.flight_id=s.flight_id
AND json_extract(latest.message,'$.Kind')='start' ORDER BY latest.id DESC LIMIT 1)
AND ` + possibleLiveStart + `))`

const iosLiveStartSubmissionSchema = `
CREATE TABLE IF NOT EXISTS ios_live_start_submissions (
  owner TEXT NOT NULL, flight_id TEXT NOT NULL, binding TEXT NOT NULL, outbox_id INTEGER,
  PRIMARY KEY(owner,flight_id,binding),
  FOREIGN KEY(owner,flight_id) REFERENCES tracked(owner,flight_id) ON DELETE CASCADE
);
INSERT OR IGNORE INTO ios_live_start_submissions(owner,flight_id,binding)
SELECT saved.owner,saved.flight_id,saved.binding FROM (` + savedIOSLiveStarts + `) saved
JOIN tracked t ON t.owner=saved.owner AND t.flight_id=saved.flight_id WHERE saved.binding<>'';
INSERT OR IGNORE INTO live_start_confirmations(device_id,flight_id)
SELECT id,flight_id FROM (` + savedIOSLiveStarts + `) WHERE binding='';
`

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
	var mayHaveSent, localActivity bool
	ios := job.target.Platform == "ios"
	err = tx.QueryRowContext(ctx, `SELECT f.snapshot,t.seat,
COALESCE(l.may_have_sent,1) OR EXISTS(SELECT 1 FROM ios_live_start_submissions a
WHERE a.owner=d.owner AND a.flight_id=o.flight_id AND a.binding=?),
d.platform='ios' AND EXISTS (
  SELECT 1 FROM devices u LEFT JOIN device_bindings ub ON ub.device_id=u.id
  WHERE u.owner=d.owner AND u.kind='live_update' AND u.flight_id=o.flight_id AND COALESCE(ub.binding,'')=?
)
FROM outbox o JOIN devices d ON d.id=o.device_id JOIN flights f ON f.id=o.flight_id
JOIN tracked t ON t.owner=d.owner AND t.flight_id=o.flight_id
LEFT JOIN outbox_live_start_leases l ON l.outbox_id=o.id
WHERE o.id=? AND o.device_id=? AND o.lease_until=? AND o.done=0`,
		job.target.Binding, job.target.Binding, job.id, job.deviceID, job.lease).Scan(&snapshot, &seat, &mayHaveSent, &localActivity)
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
	unsent := !mayHaveSent && (ios || job.attempts == 0)
	if ios && mayHaveSent || localActivity || finished(flight) || !inLiveStartWindow(flight, now) {
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
	if ios {
		if err := recordIOSLiveStart(ctx, tx, *job); err != nil {
			return false, err
		}
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

func recordIOSLiveStart(ctx context.Context, tx *sql.Tx, job deliveryJob) error {
	if job.target.Binding != "" {
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ios_live_start_submissions(owner,flight_id,binding,outbox_id) VALUES(?,?,?,?)`,
			job.owner, job.message.FlightID, job.target.Binding, job.id)
		return err
	}
	// Older clients have no stable session binding. Keep their device latch.
	return recordLiveStartConfirmation(ctx, tx, job)
}

func recordLiveStartConfirmation(ctx context.Context, tx *sql.Tx, job deliveryJob) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO live_start_confirmations SELECT device_id,flight_id
FROM live_started WHERE device_id=? AND flight_id=?`, job.deviceID, job.message.FlightID)
	return err
}

func resolveLiveStart(ctx context.Context, tx *sql.Tx, job deliveryJob, leased, done, accepted, unknown bool) error {
	ios := job.target.Platform == "ios"
	if ios && !accepted && !unknown && job.target.Binding != "" {
		// Token rotation or remapping can delete the outbox row before APNs
		// responds. Only the originating attempt can clear this binding guard.
		result, err := tx.ExecContext(ctx, `DELETE FROM ios_live_start_submissions WHERE owner=? AND flight_id=? AND binding=? AND outbox_id=?`,
			job.owner, job.message.FlightID, job.target.Binding, job.id)
		if err != nil {
			return err
		}
		if !leased {
			removed, err := result.RowsAffected()
			if err != nil || removed == 0 {
				return err
			}
			return clearLiveStartLatch(ctx, tx, job)
		}
	}
	if !leased {
		return nil
	}
	if ios && (accepted || unknown) {
		if err := recordIOSLiveStart(ctx, tx, job); err != nil {
			return err
		}
		if accepted && job.target.Binding != "" {
			return recordLiveStartConfirmation(ctx, tx, job)
		}
		return nil
	}
	if ios {
		// A rejected attempt must not be inferred as a submission on restart.
		if _, err := tx.ExecContext(ctx, `DELETE FROM live_start_confirmations WHERE device_id=? AND flight_id=?`,
			job.deviceID, job.message.FlightID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_live_start_leases VALUES(?,0)
ON CONFLICT(outbox_id) DO UPDATE SET may_have_sent=0`, job.id); err != nil {
			return err
		}
	}
	if done && !accepted {
		return clearLiveStartLatch(ctx, tx, job)
	}
	return nil
}

func clearLiveStartLatch(ctx context.Context, tx *sql.Tx, job deliveryJob) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM live_started WHERE device_id=? AND flight_id=?`, job.deviceID, job.message.FlightID)
	return err
}

func retireUnsentLiveStart(ctx context.Context, tx *sql.Tx, job deliveryJob) error {
	if err := clearLiveStartLatch(ctx, tx, job); err != nil {
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
