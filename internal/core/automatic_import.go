package core

import (
	"context"
	"database/sql"
	"time"
)

const automaticFlightReceiptLifetime = 32 * 24 * time.Hour

const automaticFlightReceiptSchema = `
CREATE TABLE IF NOT EXISTS automatic_flight_receipts (
  owner TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  digest TEXT NOT NULL, expires INTEGER NOT NULL, PRIMARY KEY(owner,digest)
);
CREATE INDEX IF NOT EXISTS automatic_flight_receipts_expiry ON automatic_flight_receipts(expires);`

// An input revision can resolve to the same flight even when its code, date,
// or departure time changes. Keep its receipt across both recurring sources
// independently of the tracking row and any particular sync subscription.
func trackAutomaticFlight(ctx context.Context, tx *sql.Tx, owner, flightID string, now, inputExpiry time.Time) error {
	digest := hash(owner + "\x00" + flightID)
	var completed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM automatic_flight_receipts WHERE owner=? AND digest=? AND expires>?
)`, owner, digest, now.Unix()).Scan(&completed); err != nil {
		return err
	}
	// Leave room for a revised date and retain the full horizon for TripIt
	// bookings that arrive more than a month before departure.
	expires := max(inputExpiry.Unix(), now.Add(automaticFlightReceiptLifetime).Unix())
	if _, err := tx.ExecContext(ctx, `INSERT INTO automatic_flight_receipts(owner,digest,expires) VALUES(?,?,?)
ON CONFLICT(owner,digest) DO UPDATE SET expires=MAX(expires,excluded.expires)`, owner, digest, expires); err != nil {
		return err
	}
	if completed {
		return nil
	}
	return trackImportedFlight(ctx, tx, owner, flightID, "", now)
}
