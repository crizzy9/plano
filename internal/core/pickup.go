package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
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

// SharedPickup resolves a pickup capability and its provider track in the
// same database snapshot. Local receiver fixes are installation-only.
func (s *Store) SharedPickup(ctx context.Context, token string, now time.Time) (feeds.Flight, []feeds.Position, error) {
	var flight feeds.Flight
	var snapshot, telemetry []byte
	err := s.db.QueryRowContext(ctx, `SELECT f.snapshot,f.telemetry FROM shares s
JOIN tracked t ON t.owner=s.owner AND t.flight_id=s.flight_id
JOIN flights f ON f.id=s.flight_id
WHERE s.token_hash=? AND s.expires>?`, hash(token), now.UnixNano()).Scan(&snapshot, &telemetry)
	if errors.Is(err, sql.ErrNoRows) {
		return flight, nil, ErrNotFound
	}
	if err != nil {
		return flight, nil, err
	}
	if err := json.Unmarshal(snapshot, &flight); err != nil {
		return flight, nil, err
	}
	if flight.Status == "cancelled" || (flight.Source != "flightaware_aeroapi" && flight.Source != "plano_demo") {
		return flight, nil, nil
	}
	var positions []feeds.Position
	if err := json.Unmarshal(telemetry, &positions); err != nil {
		return flight, nil, err
	}
	return flight, pickupTrack(flight, positions, now), nil
}

func pickupTrack(flight feeds.Flight, positions []feeds.Position, now time.Time) []feeds.Position {
	start, end := now.Add(-25*time.Hour), now.Add(30*time.Second)
	if flight.ActualTakeoff != nil && flight.ActualTakeoff.After(start) {
		start = *flight.ActualTakeoff
	}
	if flight.ActualLanding != nil && flight.ActualLanding.Before(end) {
		end = *flight.ActualLanding
	}
	positions = slices.DeleteFunc(positions, func(p feeds.Position) bool {
		return p.Source != flight.Source || p.At.Before(start) || p.At.After(end) ||
			p.Latitude < -90 || p.Latitude > 90 || p.Longitude < -180 || p.Longitude > 180
	})
	slices.SortStableFunc(positions, func(a, b feeds.Position) int { return a.At.Compare(b.At) })
	positions = slices.CompactFunc(positions, func(a, b feeds.Position) bool { return a.At.Equal(b.At) })
	// Preserve the observation cadence for gap detection before rendering.
	for i, p := range positions {
		positions[i] = feeds.Position{Latitude: p.Latitude, Longitude: p.Longitude, At: p.At.UTC()}
	}
	return positions
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
