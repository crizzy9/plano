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
	var raw, snapshot []byte
	err := s.db.QueryRowContext(ctx, `SELECT f.inbound,f.snapshot FROM tracked t JOIN flights f ON f.id=t.flight_id
WHERE t.owner=? AND t.flight_id=?`, owner, parent).Scan(&raw, &snapshot)
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
			if i != 0 {
				return leg, false, nil
			}
			var onward feeds.Flight
			if err := json.Unmarshal(snapshot, &onward); err != nil {
				return feeds.Flight{}, false, err
			}
			if linksAeroInbound(onward, leg) && leg.AeroProviderID() != "" {
				arrival, _ := inboundArrival(leg)
				compatibleTiming := arrival == nil || onward.ScheduledDeparture == nil || inboundChronologyCompatible(leg, *arrival, *onward.ScheduledDeparture)
				return leg, connectsAeroInbound(onward, leg) && compatibleTiming, nil
			}
			return leg, true, nil
		}
	}
	return feeds.Flight{}, false, ErrNotFound
}
