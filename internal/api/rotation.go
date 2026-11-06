package api

import (
	"context"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) rotationDetails(ctx context.Context, owner, parent, id string, now time.Time) (core.Details, error) {
	leg, immediate, local, err := s.Store.TrackedRotationRadar(ctx, owner, parent, id, now)
	if err != nil {
		return core.Details{}, err
	}
	details := core.Details{Flight: leg, Events: []core.Event{}, Telemetry: local}
	if s.Source == nil || !immediate || !rotationTrackCurrent(leg, now) {
		return details, nil
	}
	trackCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	points, trackErr := s.Source.Track(trackCtx, leg.ID)
	// The tracked relationship or operating aircraft can change during the request.
	now = time.Now()
	current, immediate, local, err := s.Store.TrackedRotationRadar(ctx, owner, parent, id, now)
	if err != nil {
		return core.Details{}, err
	}
	details.Flight, details.Telemetry = current, local
	takeoffMatches := leg.ActualTakeoff == nil && current.ActualTakeoff == nil ||
		leg.ActualTakeoff != nil && current.ActualTakeoff != nil && leg.ActualTakeoff.Equal(*current.ActualTakeoff)
	if trackErr == nil && immediate && rotationTrackCurrent(current, now) && current.TailNumber == leg.TailNumber &&
		current.Source == leg.Source && current.AeroProviderID() == leg.AeroProviderID() && takeoffMatches {
		details.Telemetry = core.MergeFlightPositions(points, local)
	}
	if len(details.Telemetry) > 256 {
		details.Telemetry = details.Telemetry[len(details.Telemetry)-256:]
	}
	return details, nil
}

func rotationTrackCurrent(leg feeds.Flight, now time.Time) bool {
	return leg.Status == "airborne" && leg.ActualLanding == nil && leg.ActualArrival == nil &&
		!leg.ObservedAt.After(now) && now.Sub(leg.ObservedAt) < 30*time.Minute
}
