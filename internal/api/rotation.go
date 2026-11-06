package api

import (
	"context"
	"time"

	"github.com/crizzy9/plano/internal/core"
)

func (s Server) rotationDetails(ctx context.Context, owner, parent, id string, now time.Time) (core.Details, error) {
	leg, immediate, err := s.Store.TrackedRotationLeg(ctx, owner, parent, id)
	if err != nil {
		return core.Details{}, err
	}
	details := core.Details{Flight: leg, Events: []core.Event{}}
	if s.Source == nil || !immediate || leg.Status != "airborne" || leg.ObservedAt.After(now) || now.Sub(leg.ObservedAt) >= 30*time.Minute {
		return details, nil
	}
	trackCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	points, trackErr := s.Source.Track(trackCtx, leg.ID)
	// The tracked relationship or operating aircraft can change during the request.
	current, immediate, err := s.Store.TrackedRotationLeg(ctx, owner, parent, id)
	if err != nil {
		return core.Details{}, err
	}
	details.Flight = current
	takeoffMatches := leg.ActualTakeoff == nil && current.ActualTakeoff == nil ||
		leg.ActualTakeoff != nil && current.ActualTakeoff != nil && leg.ActualTakeoff.Equal(*current.ActualTakeoff)
	if trackErr == nil && immediate && current.Status == "airborne" && current.ActualLanding == nil &&
		current.ActualArrival == nil && current.TailNumber == leg.TailNumber && takeoffMatches {
		if len(points) > 256 {
			points = points[len(points)-256:]
		}
		details.Telemetry = points
	}
	return details, nil
}
