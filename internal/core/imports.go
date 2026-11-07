package core

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

var ErrEmailRelayUnauthorized = errors.New("email relay is no longer authorized")

const emailIdentitySchema = `
CREATE TABLE IF NOT EXISTS email_import_identities (
  owner TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  identity TEXT NOT NULL, digest TEXT NOT NULL, seen INTEGER NOT NULL, PRIMARY KEY(owner,identity)
);
CREATE INDEX IF NOT EXISTS email_import_identities_by_seen ON email_import_identities(seen);
`

const emailIdentityIdleTTL = 24 * time.Hour

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

// BindEmailImportDigest gives equivalent deliveries one receipt key before
// lookups begin. An envelope alias keeps exact replays stable across parser
// revisions, and a legacy delivery can carry forward its raw-byte key.
func (s *Store) BindEmailImportDigest(ctx context.Context, owner, relayToken, rawDigest, fingerprint string) (string, error) {
	if relayToken == "" || !emailImportHash(rawDigest) || fingerprint != "" && (rawDigest == fingerprint || !emailImportHash(fingerprint)) {
		return "", feeds.ErrInvalidQuery
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	allowed, err := emailRelayAllowed(ctx, tx, owner, relayToken)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", ErrEmailRelayUnauthorized
	}
	seen := time.Now().Unix()
	envelope := hash("plano-email-envelope-v1\x00" + rawDigest)
	const bindIdentity = `INSERT INTO email_import_identities(owner,identity,digest,seen)
VALUES(?,?,?,?)
ON CONFLICT(owner,identity) DO UPDATE SET seen=MAX(email_import_identities.seen,excluded.seen)`
	var bound string
	err = tx.QueryRowContext(ctx, `UPDATE email_import_identities SET seen=MAX(seen,?)
WHERE owner=? AND identity=? RETURNING digest`, seen, owner, envelope).Scan(&bound)
	if err == nil {
		if fingerprint != "" {
			// An exact replay can connect a newly parsed fingerprint to the
			// original receipt without changing an established binding.
			if _, err := tx.ExecContext(ctx, bindIdentity, owner, fingerprint, bound, seen); err != nil {
				return "", err
			}
		}
		return bound, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if fingerprint == "" {
		return rawDigest, nil
	}
	var legacy bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM imported_messages WHERE owner=?1 AND digest=?2)
  OR EXISTS(SELECT 1 FROM email_import_legs WHERE owner=?1 AND digest=?2)`, owner, rawDigest).Scan(&legacy)
	if err != nil {
		return "", err
	}
	digest := fingerprint
	if legacy {
		digest = rawDigest
	}
	err = tx.QueryRowContext(ctx, bindIdentity+` RETURNING digest`, owner, fingerprint, digest, seen).Scan(&bound)
	if err != nil {
		return "", err
	}
	if legacy {
		// Exact replays retain their progress even if another delivery already
		// established the binding. Leave the binding in place for variants.
		bound = rawDigest
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO email_import_identities(owner,identity,digest,seen) VALUES(?,?,?,?)`,
		owner, envelope, bound, seen); err != nil {
		return "", err
	}
	return bound, tx.Commit()
}

func pruneEmailImportIdentities(ctx context.Context, tx *sql.Tx, now time.Time) error {
	// Keep bindings with progress and allow in-flight lookups time to commit.
	_, err := tx.ExecContext(ctx, `DELETE FROM email_import_identities AS i WHERE seen<=?
AND NOT EXISTS (SELECT 1 FROM imported_messages WHERE owner=i.owner AND digest=i.digest)
AND NOT EXISTS (SELECT 1 FROM email_import_legs WHERE owner=i.owner AND digest=i.digest)`, now.Add(-emailIdentityIdleTTL).Unix())
	return err
}

func (s *Store) ImportedEmail(ctx context.Context, owner, digest string) (bool, error) {
	return importedEmail(ctx, s.db, owner, digest)
}

func importedEmail(ctx context.Context, db rowQueryer, owner, digest string) (bool, error) {
	var found bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM imported_messages WHERE owner=? AND digest=?)`, owner, digest).Scan(&found)
	return found, err
}

type EmailImportLeg struct{ Key, FlightID string }

type emailImportRowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// EmailImportProgress reports the accepted leg hashes for a partial message.
// Completed messages use ImportedEmail instead.
func (s *Store) EmailImportProgress(ctx context.Context, owner, digest string) (map[string]bool, error) {
	receipts, err := emailImportReceipts(ctx, s.db, owner, digest)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(receipts))
	for key := range receipts {
		seen[key] = true
	}
	return seen, nil
}

func emailImportReceipts(ctx context.Context, db emailImportRowsQueryer, owner, digest string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT leg_hash,flight_hash FROM email_import_legs WHERE owner=? AND digest=?`, owner, digest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	receipts := make(map[string]string)
	for rows.Next() {
		var key, flightHash string
		if err := rows.Scan(&key, &flightHash); err != nil {
			return nil, err
		}
		receipts[key] = flightHash
	}
	return receipts, rows.Err()
}

// CommitEmailImport adds only newly accepted legs. Receipts survive untracking
// and relay rotation. The current relay is required even on a completed replay.
func (s *Store) CommitEmailImport(ctx context.Context, owner, digest, relayToken, locator string, wanted []string, resolved []EmailImportLeg) (complete, partial bool, err error) {
	if relayToken == "" || !emailImportHash(digest) || locator != "" && !emailImportHash(locator) || !validEmailImportLegs(wanted, resolved) {
		return false, false, feeds.ErrInvalidQuery
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()
	allowed, err := emailRelayAllowed(ctx, tx, owner, relayToken)
	if err != nil {
		return false, false, err
	}
	if !allowed {
		return false, false, ErrEmailRelayUnauthorized
	}
	if done, err := importedEmail(ctx, tx, owner, digest); err != nil || done {
		return done, false, err
	}
	receipts, err := emailImportReceipts(ctx, tx, owner, digest)
	if err != nil {
		return false, false, err
	}
	acceptedFlights := make(map[string]bool, len(receipts))
	for _, flightHash := range receipts {
		acceptedFlights[flightHash] = true
	}
	now := time.Now()
	for _, leg := range resolved {
		if _, consumed := receipts[leg.Key]; consumed {
			continue
		}
		flightHash := hash(owner + "\x00" + digest + "\x00" + leg.FlightID)
		if !acceptedFlights[flightHash] {
			if err := trackImportedFlight(ctx, tx, owner, leg.FlightID, locator, now); err != nil {
				return false, false, err
			}
			acceptedFlights[flightHash] = true
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO email_import_legs(owner,digest,leg_hash,flight_hash) VALUES(?,?,?,?)`, owner, digest, leg.Key, flightHash); err != nil {
			return false, false, err
		}
		receipts[leg.Key] = flightHash
	}
	complete = true
	for _, key := range wanted {
		if _, consumed := receipts[key]; !consumed {
			complete = false
			break
		}
	}
	if complete {
		if _, err := tx.ExecContext(ctx, `INSERT INTO imported_messages VALUES(?,?,?)`, owner, digest, now.Unix()); err != nil {
			return false, false, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM email_import_legs WHERE owner=? AND digest=?`, owner, digest); err != nil {
			return false, false, err
		}
	}
	if err := s.commitOutbox(tx); err != nil {
		return false, false, err
	}
	return complete, !complete && len(receipts) > 0, nil
}

func validEmailImportLegs(wanted []string, resolved []EmailImportLeg) bool {
	const maxEmailImportLegs = 8
	if len(wanted) == 0 || len(wanted) > maxEmailImportLegs || len(resolved) > len(wanted) {
		return false
	}
	keys := make(map[string]bool, len(wanted))
	for _, key := range wanted {
		if !emailImportHash(key) || keys[key] {
			return false
		}
		keys[key] = true
	}
	for _, leg := range resolved {
		if leg.FlightID == "" || !keys[leg.Key] {
			return false
		}
		delete(keys, leg.Key)
	}
	return true
}

func emailImportHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	var decoded [32]byte
	_, err := hex.Decode(decoded[:], []byte(value))
	return err == nil
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
		allowed, err := emailRelayAllowed(ctx, tx, owner, relayToken)
		if err != nil {
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
		if err := trackImportedFlight(ctx, tx, owner, f.ID, locator, now); err != nil {
			return false, err
		}
	}
	return false, s.commitOutbox(tx)
}

func emailRelayAllowed(ctx context.Context, tx *sql.Tx, owner, token string) (bool, error) {
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM email_relays WHERE owner=? AND token_hash=?)`, owner, hash(token)).Scan(&allowed)
	return allowed, err
}

func trackImportedFlight(ctx context.Context, tx *sql.Tx, owner, id, locator string, now time.Time) error {
	if err := trackFlight(ctx, tx, owner, id, "", now); err != nil {
		return err
	}
	if locator != "" {
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO bookings(owner,flight_id,locator_hash) VALUES(?,?,?)`, owner, id, locator)
		return err
	}
	return nil
}

func (s *Store) BookedFlights(ctx context.Context, owner, locator string) ([]feeds.Flight, error) {
	return s.list(ctx, `SELECT f.snapshot FROM bookings b JOIN flights f ON f.id=b.flight_id WHERE b.owner=? AND b.locator_hash=? ORDER BY f.observed_ns DESC`, owner, locator)
}
