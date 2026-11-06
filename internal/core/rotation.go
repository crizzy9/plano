package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/crizzy9/plano/internal/feeds"
)

// TrackedRotationLeg resolves a saved leg through the caller's tracked flight.
// The first result in a rotation is the immediate inbound.
func (s *Store) TrackedRotationLeg(ctx context.Context, owner, parent, id string) (feeds.Flight, bool, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT f.inbound FROM tracked t JOIN flights f ON f.id=t.flight_id
WHERE t.owner=? AND t.flight_id=?`, owner, parent).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return feeds.Flight{}, false, ErrNotFound
	}
	if err != nil {
		return feeds.Flight{}, false, err
	}
	var legs []feeds.Flight
	if err := json.Unmarshal(raw, &legs); err != nil {
		return feeds.Flight{}, false, err
	}
	for i, leg := range legs {
		if leg.ID == id {
			return leg, i == 0, nil
		}
	}
	return feeds.Flight{}, false, ErrNotFound
}
