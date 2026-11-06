package core

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func (s *Store) RotateEmailRelay(ctx context.Context, owner string) (string, error) {
	token := randomToken()
	_, err := s.db.ExecContext(ctx, `INSERT INTO email_relays VALUES(?,?) ON CONFLICT(owner) DO UPDATE SET token_hash=excluded.token_hash`, owner, hash(token))
	return token, err
}

func (s *Store) RemoveEmailRelay(ctx context.Context, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM email_relays WHERE owner=?`, owner)
	return err
}

func (s *Store) EmailRelayOwner(ctx context.Context, token string) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT owner FROM email_relays WHERE token_hash=?`, hash(token)).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return owner, err
}

func (s *Store) ImportedEmail(ctx context.Context, owner, digest string) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM imported_messages WHERE owner=? AND digest=?)`, owner, digest).Scan(&found)
	return found, err
}

// CommitImport adds the resolved legs and their optional private booking index
// as one transaction. A webhook key is checked again inside the write lock.
func (s *Store) CommitImport(ctx context.Context, owner string, flights []feeds.Flight, locator, digest, relayToken string) (duplicate bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now()
	if relayToken != "" {
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM email_relays WHERE owner=? AND token_hash=?)`, owner, hash(relayToken)).Scan(&allowed); err != nil {
			return false, err
		}
		if !allowed {
			return false, ErrNotFound
		}
	}
	if digest != "" {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO imported_messages VALUES(?,?,?)`, owner, digest, now.Unix())
		if err != nil {
			return false, err
		}
		count, err := res.RowsAffected()
		if err != nil || count == 0 {
			return count == 0, err
		}
	}
	for _, f := range flights {
		if err := trackFlight(ctx, tx, owner, f.ID, "", now); err != nil {
			return false, err
		}
		if locator != "" {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO bookings(owner,flight_id,locator_hash) VALUES(?,?,?)`, owner, f.ID, locator); err != nil {
				return false, err
			}
		}
	}
	return false, tx.Commit()
}

func (s *Store) BookedFlights(ctx context.Context, owner, locator string) ([]feeds.Flight, error) {
	return s.list(ctx, `SELECT f.snapshot FROM bookings b JOIN flights f ON f.id=b.flight_id WHERE b.owner=? AND b.locator_hash=? ORDER BY f.observed_ns DESC`, owner, locator)
}
