package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"time"
)

const pickupSchema = `
CREATE TABLE IF NOT EXISTS pickup_link_meta (
  id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE REFERENCES shares(token_hash) ON DELETE CASCADE,
  created_ns INTEGER
);
INSERT OR IGNORE INTO pickup_link_meta(id,token_hash)
SELECT lower(hex(randomblob(16))),s.token_hash FROM shares s
WHERE NOT EXISTS(SELECT 1 FROM pickup_link_meta m WHERE m.token_hash=s.token_hash);
CREATE INDEX IF NOT EXISTS pickup_share_owner ON shares(owner,flight_id,expires);
`

type PickupLink struct {
	ID        string     `json:"id"`
	FlightID  string     `json:"flightId"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	ExpiresAt time.Time  `json:"expiresAt"`
}

func (s *Store) CreatePickupLink(ctx context.Context, owner, flightID string, now, expires time.Time) (PickupLink, string, error) {
	var random [16]byte
	rand.Read(random[:])
	token := randomToken()
	tokenHash := hash(token)
	link := PickupLink{ID: hex.EncodeToString(random[:]), FlightID: flightID, CreatedAt: &now, ExpiresAt: expires}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PickupLink{}, "", err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO shares SELECT ?,owner,flight_id,? FROM tracked WHERE owner=? AND flight_id=?`, tokenHash, expires.UnixNano(), owner, flightID)
	if err != nil {
		return PickupLink{}, "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return PickupLink{}, "", err
	}
	if n == 0 {
		return PickupLink{}, "", ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pickup_link_meta VALUES(?,?,?)`, link.ID, tokenHash, now.UnixNano()); err != nil {
		return PickupLink{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return PickupLink{}, "", err
	}
	return link, token, nil
}

func (s *Store) PickupLinks(ctx context.Context, owner, flightID string, now time.Time) ([]PickupLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,s.flight_id,m.created_ns,s.expires FROM shares s
JOIN pickup_link_meta m ON m.token_hash=s.token_hash WHERE s.owner=? AND s.flight_id=? AND s.expires>?
ORDER BY m.created_ns DESC,m.id`, owner, flightID, now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := make([]PickupLink, 0)
	for rows.Next() {
		var link PickupLink
		var created sql.NullInt64
		var expires int64
		if err := rows.Scan(&link.ID, &link.FlightID, &created, &expires); err != nil {
			return nil, err
		}
		if created.Valid {
			at := time.Unix(0, created.Int64).UTC()
			link.CreatedAt = &at
		}
		link.ExpiresAt = time.Unix(0, expires).UTC()
		links = append(links, link)
	}
	return links, rows.Err()
}

func (s *Store) RevokePickupLink(ctx context.Context, owner, linkID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM shares WHERE owner=? AND token_hash=(SELECT token_hash FROM pickup_link_meta WHERE id=?)`, owner, linkID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}
