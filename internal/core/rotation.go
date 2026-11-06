package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/crizzy9/plano/internal/feeds"
)

type trackedRotationLeg struct {
	onward, flight feeds.Flight
	providerID     string
	immediate      bool
}

func readTrackedRotationLeg(ctx context.Context, db rowQueryer, owner, parent, id string) (trackedRotationLeg, error) {
	var raw, snapshot []byte
	var providerID string
	err := db.QueryRowContext(ctx, `SELECT f.inbound,f.snapshot,COALESCE(k.provider_id,'')
FROM tracked t JOIN flights f ON f.id=t.flight_id LEFT JOIN aero_instance_keys k ON k.flight_id=f.id
WHERE t.owner=? AND t.flight_id=?`, owner, parent).Scan(&raw, &snapshot, &providerID)
	if errors.Is(err, sql.ErrNoRows) {
		return trackedRotationLeg{}, ErrNotFound
	}
	if err != nil {
		return trackedRotationLeg{}, err
	}
	var legs []feeds.Flight
	if err := json.Unmarshal(raw, &legs); err != nil {
		return trackedRotationLeg{}, err
	}
	for i, leg := range legs {
		if leg.ID != id {
			continue
		}
		result := trackedRotationLeg{flight: leg, providerID: providerID, immediate: i == 0}
		if !result.immediate {
			return result, nil
		}
		if err := json.Unmarshal(snapshot, &result.onward); err != nil {
			return trackedRotationLeg{}, err
		}
		if linksAeroInbound(result.onward, leg) && leg.AeroProviderID() != "" {
			arrival, _ := inboundArrival(leg)
			departure := result.onward.ScheduledDeparture
			compatibleTiming := arrival == nil || departure == nil || inboundChronologyCompatible(leg, *arrival, *departure)
			result.immediate = connectsAeroInbound(result.onward, leg) && compatibleTiming
		}
		return result, nil
	}
	return trackedRotationLeg{}, ErrNotFound
}
