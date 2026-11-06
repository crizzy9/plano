package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const pollRotationTimeout = 20 * time.Second

// Refresher polls tracked flights in an active window with up to two concurrent
// flight refreshes. A feed outage retains earlier observations.
type Refresher struct {
	Store      *Store
	Flights    feeds.FlightSource
	Advisories []feeds.AdvisorySource
	Logger     *slog.Logger
	Interval   time.Duration
}

func (r Refresher) Run(ctx context.Context) {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		if err := r.refresh(ctx); err != nil && ctx.Err() == nil {
			r.Logger.Error("could not refresh tracked flights", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r Refresher) refresh(ctx context.Context) error {
	now := time.Now()
	active, err := r.Store.Active(ctx, now)
	if err != nil {
		return err
	}
	if len(active) == 0 {
		return r.Store.Prune(ctx, now)
	}
	var advisories []feeds.Advisory
	for _, source := range r.Advisories {
		result, err := source.Advisories(ctx)
		if err != nil {
			r.Logger.Warn("advisory source unavailable", "error", err)
			continue
		}
		if named, ok := source.(interface{ AdvisorySourceID() string }); ok {
			id := named.AdvisorySourceID()
			if err := r.Store.cachePolledFlightNotices(ctx, id, result, time.Now()); err != nil && ctx.Err() == nil {
				r.Logger.Warn("could not cache flight notices", "source", id, "error", err)
				if errors.Is(err, feeds.ErrInvalidQuery) {
					continue
				}
				if slices.ContainsFunc(result, func(a feeds.Advisory) bool {
					return a.SupersedesID != "" || a.IsTFMSProgramCancellation()
				}) {
					return err
				}
			}
		}
		if advisories == nil {
			advisories = make([]feeds.Advisory, 0)
		}
		for _, advisory := range result {
			if !r.Store.ignoreLegacyTFMSReplay(advisory) {
				advisories = append(advisories, advisory)
			}
		}
	}
	// One slow provider lookup can occupy a worker while the other continues
	// through the queue. Store.observe still commits each flight atomically.
	jobs := make(chan feeds.Flight)
	var workers sync.WaitGroup
	var firstError error
	var recordError sync.Once
	for range min(2, len(active)) {
		workers.Go(func() {
			for prior := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := r.refreshFlight(ctx, prior, advisories); err != nil {
					recordError.Do(func() {
						firstError = fmt.Errorf("save observation for flight %s: %w", prior.ID, err)
					})
				}
			}
		})
	}
dispatch:
	for _, prior := range active {
		if finished(prior) && prior.ObservedAt.Add(5*time.Minute).After(now) {
			continue
		}
		select {
		case jobs <- prior:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(firstError, r.Store.Prune(ctx, now))
}

func (r Refresher) refreshFlight(ctx context.Context, prior feeds.Flight, advisories []feeds.Advisory) error {
	flight, err := r.Flights.Get(ctx, prior.ID)
	if err != nil {
		r.Logger.Warn("flight refresh failed", "id", prior.ID, "error", err)
		return nil
	}
	var positions []feeds.Position
	if (flight.ActualDeparture != nil || flight.ActualTakeoff != nil || flight.Status == "airborne") &&
		(!finished(prior) || prior.ActualArrival == nil) {
		trackID := flight.AeroProviderID()
		if trackID == "" {
			trackID = flight.ID
		}
		positions, err = r.Flights.Track(ctx, trackID)
		if err != nil {
			r.Logger.Warn("flight track refresh failed", "id", flight.ID, "error", err)
		}
	}
	var inbound []feeds.Flight
	var scope aeroObservationScope
	if flightBeforeDeparture(flight) && (flight.InboundID != "" || flight.TailNumber != "") {
		rotationCtx, cancel := context.WithTimeout(ctx, pollRotationTimeout)
		inbound, err = r.Flights.Rotation(rotationCtx, flight)
		cancel()
		if err != nil {
			r.Logger.Warn("aircraft rotation refresh failed", "id", flight.ID, "error", err)
			if len(inbound) == 0 {
				inbound = nil
			} else {
				scope.recoverRotationSuffix = feeds.RotationSuffixRecoverable(err)
			}
		}
	}
	return r.Store.observe(ctx, flight, positions, inbound, advisories, observationPoll, scope)
}
