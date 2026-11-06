package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"
)

const tripItReceiptColumns = `(
  owner TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  leg_key TEXT NOT NULL, date TEXT NOT NULL, PRIMARY KEY(owner,leg_key)
)`

const tripItReceiptIndex = `CREATE INDEX IF NOT EXISTS tripit_imported_legs_by_date ON tripit_imported_legs(date);`

const tripItSyncSchema = `
CREATE TABLE IF NOT EXISTS tripit_subscriptions (
  owner TEXT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
  generation TEXT NOT NULL, secret BLOB NOT NULL, due_ns INTEGER NOT NULL,
  lease_ns INTEGER NOT NULL DEFAULT 0, checked_ns INTEGER,
  status TEXT NOT NULL DEFAULT 'pending', cursor INTEGER NOT NULL DEFAULT 0,
  matched INTEGER NOT NULL DEFAULT 0, unmatched INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS tripit_sync_due ON tripit_subscriptions(due_ns);
CREATE TABLE IF NOT EXISTS tripit_imported_legs ` + tripItReceiptColumns + `;
` + tripItReceiptIndex

// Older databases tied exact-key receipts to a subscription. Move the foreign
// key to the installation so stopping sync can erase the feed without making
// those keys importable again. Keep the existing table name and row shape.
func migrateTripItReceipts(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var legacy bool
	if err := tx.QueryRow(`SELECT EXISTS (
  SELECT 1 FROM pragma_foreign_key_list('tripit_imported_legs')
  WHERE "table"='tripit_subscriptions' AND "from"='owner' AND "to"='owner'
)`).Scan(&legacy); err != nil {
		return err
	}
	if legacy {
		if _, err := tx.Exec(`CREATE TABLE tripit_imported_legs_new ` + tripItReceiptColumns + `;
INSERT INTO tripit_imported_legs_new(owner,leg_key,date) SELECT owner,leg_key,date FROM tripit_imported_legs;
DROP TABLE tripit_imported_legs;
ALTER TABLE tripit_imported_legs_new RENAME TO tripit_imported_legs;
` + tripItReceiptIndex); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type TripItStatus struct {
	Enabled    bool       `json:"enabled"`
	NextSyncAt *time.Time `json:"nextSyncAt,omitempty"`
	LastSyncAt *time.Time `json:"lastSyncAt,omitempty"`
	Status     string     `json:"status,omitempty"`
	Matched    int        `json:"matched,omitempty"`
	Unmatched  int        `json:"unmatched,omitempty"`
}

type TripItJob struct {
	Owner, Generation string
	EncryptedURL      []byte
	Lease             int64
	Cursor            int
}

type TripItMatch struct{ Key, Date, FlightID string }

func (s *Store) SetTripItSubscription(ctx context.Context, owner string, encryptedURL []byte, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tripit_subscriptions(owner,generation,secret,due_ns) VALUES(?,?,?,?)
ON CONFLICT(owner) DO UPDATE SET generation=excluded.generation,secret=excluded.secret,due_ns=excluded.due_ns,
lease_ns=0,checked_ns=NULL,status='pending',cursor=0,matched=0,unmatched=0`, owner, rand.Text(), encryptedURL, now.UnixNano())
	return err
}

func (s *Store) RemoveTripItSubscription(ctx context.Context, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tripit_subscriptions WHERE owner=?`, owner)
	return err
}

func (s *Store) TripItStatus(ctx context.Context, owner string) (TripItStatus, error) {
	var status TripItStatus
	var due int64
	var checked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT due_ns,checked_ns,status,matched,unmatched FROM tripit_subscriptions WHERE owner=?`, owner).Scan(&due, &checked, &status.Status, &status.Matched, &status.Unmatched)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	status.Enabled = true
	next := time.Unix(0, due).UTC()
	status.NextSyncAt = &next
	if checked.Valid {
		at := time.Unix(0, checked.Int64).UTC()
		status.LastSyncAt = &at
	}
	return status, nil
}

func (s *Store) ClaimTripItSync(ctx context.Context, now time.Time) (*TripItJob, error) {
	job := TripItJob{Lease: now.Add(time.Minute).UnixNano()}
	err := s.db.QueryRowContext(ctx, `UPDATE tripit_subscriptions SET lease_ns=? WHERE owner=(
SELECT owner FROM tripit_subscriptions WHERE due_ns<=? AND lease_ns<=? ORDER BY due_ns,owner LIMIT 1)
RETURNING owner,generation,secret,cursor`, job.Lease, now.UnixNano(), now.UnixNano()).Scan(&job.Owner, &job.Generation, &job.EncryptedURL, &job.Cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &job, err
}

func (s *Store) TripItImportedLegs(ctx context.Context, owner string, from string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT leg_key FROM tripit_imported_legs WHERE owner=? AND date>=?`, owner, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[string]bool)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		seen[key] = true
	}
	return seen, rows.Err()
}

type TripItSyncResult struct {
	Matches                    []TripItMatch
	Matched, Unmatched, Cursor int
	Unavailable                bool
}

// FinishTripItSync verifies the leased generation before importing any legs.
// Disabling or replacing a feed fences off a request already on the network.
func (s *Store) FinishTripItSync(ctx context.Context, job TripItJob, result TripItSyncResult, now, next time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	status := "synced"
	if result.Unavailable {
		status = "unavailable"
	}
	updated, err := tx.ExecContext(ctx, `UPDATE tripit_subscriptions SET due_ns=?,lease_ns=0,checked_ns=?,status=?,cursor=?,matched=?,unmatched=?
WHERE owner=? AND generation=? AND lease_ns=?`, next.UnixNano(), now.UnixNano(), status, result.Cursor, result.Matched, result.Unmatched, job.Owner, job.Generation, job.Lease)
	if err != nil {
		return err
	}
	n, err := updated.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	for _, match := range result.Matches {
		inserted, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tripit_imported_legs VALUES(?,?,?)`, job.Owner, match.Key, match.Date)
		if err != nil {
			return err
		}
		count, err := inserted.RowsAffected()
		if err != nil {
			return err
		}
		if count != 0 {
			day, err := time.Parse(time.DateOnly, match.Date)
			if err != nil {
				return err
			}
			if err := trackAutomaticFlight(ctx, tx, job.Owner, match.FlightID, now, day.AddDate(0, 0, 3)); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tripit_imported_legs WHERE owner=? AND date<?`, job.Owner, now.AddDate(0, 0, -2).Format(time.DateOnly)); err != nil {
		return err
	}
	return s.commitOutbox(tx)
}
