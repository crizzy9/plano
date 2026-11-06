package core

import (
	"context"
	"time"
)

const calendarSyncSchema = `
CREATE TABLE IF NOT EXISTS calendar_import_receipts (
  owner TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  digest TEXT NOT NULL, expires INTEGER NOT NULL, PRIMARY KEY(owner, digest)
);
CREATE INDEX IF NOT EXISTS calendar_import_receipts_expiry ON calendar_import_receipts(expires);`

type CalendarImportLeg struct {
	Key, FlightID string
	ExpiresAt     time.Time
}

func calendarImportDigest(owner, key string) string {
	return hash(owner + "\x00" + key)
}

func (s *Store) CalendarImportCompleted(ctx context.Context, owner, key string) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM calendar_import_receipts WHERE owner=? AND digest=?)`,
		owner, calendarImportDigest(owner, key)).Scan(&found)
	return found, err
}

// CommitCalendarImport saves receipts in the same transaction as tracking.
// Receipts survive untracking, including when two deliveries resolve together.
func (s *Store) CommitCalendarImport(ctx context.Context, owner string, legs []CalendarImportLeg) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	tracked := make(map[string]bool, len(legs))
	for _, leg := range legs {
		res, err := tx.ExecContext(ctx, `INSERT INTO calendar_import_receipts(owner,digest,expires) VALUES(?,?,?)
ON CONFLICT(owner,digest) DO NOTHING`, owner, calendarImportDigest(owner, leg.Key), leg.ExpiresAt.Unix())
		if err != nil {
			return err
		}
		count, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 || tracked[leg.FlightID] {
			continue
		}
		if err := trackImportedFlight(ctx, tx, owner, leg.FlightID, "", now); err != nil {
			return err
		}
		tracked[leg.FlightID] = true
	}
	return s.commitOutbox(tx)
}
