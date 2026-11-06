package core

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

type weatherFixture func(context.Context, []string, time.Time) ([]feeds.Advisory, error)

func (f weatherFixture) Observations(ctx context.Context, stations []string, now time.Time) ([]feeds.Advisory, error) {
	return f(ctx, stations, now)
}

func TestWeatherBatchesTrackedOperationsAndExpiresCachedObservations(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "weather.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	flight := feeds.Flight{ID: "weather-flight", Number: "UA123", Status: "scheduled", Source: "fixture",
		OriginICAO: "KSFO", DestinationICAO: "KJFK", ScheduledDeparture: new(now.Add(time.Hour)),
		ScheduledArrival: new(now.Add(6 * time.Hour)), ObservedAt: now}
	other := flight
	other.ID, other.OriginICAO = "untracked-weather-flight", "KDEN"
	for _, snapshot := range []feeds.Flight{flight, other} {
		if err := s.Observe(ctx, snapshot, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	var calls int
	cache := NewWeatherCache(weatherFixture(func(_ context.Context, stations []string, _ time.Time) ([]feeds.Advisory, error) {
		if !slices.Equal(stations, []string{"KSFO"}) {
			t.Fatalf("unexpected station batch %v", stations)
		}
		calls++
		switch calls {
		case 1:
			return []feeds.Advisory{{Airport: "KSFO", Kind: feeds.METARWeatherObservation, Source: feeds.METARSource,
				Reason: "low visibility", ObservedAt: now.Add(-15 * time.Minute), EndsAt: new(now.Add(75 * time.Minute))},
				{Airport: "KJFK", Reason: "unrequested report", ObservedAt: now, EndsAt: new(now.Add(time.Hour))}}, nil
		case 2:
			return nil, feeds.ErrUnavailable
		default:
			return nil, nil
		}
	}))
	if err := cache.refresh(ctx, s, now); err != nil {
		t.Fatal(err)
	}
	reports := cache.ForFlight(flight, now)
	if len(reports) != 1 || reports[0].Operation != "departure" || reports[0].Conditions != "low visibility" || !reports[0].ObservedAt.Equal(now.Add(-15*time.Minute)) {
		t.Fatalf("unexpected reports %+v", reports)
	}
	if err := cache.refresh(ctx, s, now.Add(weatherCadence)); !errors.Is(err, feeds.ErrUnavailable) {
		t.Fatalf("unexpected outage error %v", err)
	}
	if got := cache.ForFlight(flight, now.Add(weatherCadence)); len(got) != 1 {
		t.Fatalf("lost valid report after outage: %+v", got)
	}
	if got := cache.ForFlight(flight, now.Add(75*time.Minute)); len(got) != 0 {
		t.Fatalf("expired report still visible: %+v", got)
	}
	if err := cache.refresh(ctx, s, now.Add(2*weatherCadence)); err != nil {
		t.Fatal(err)
	}
	if got := cache.ForFlight(flight, now.Add(2*weatherCadence)); len(got) != 0 || calls != 3 {
		t.Fatalf("successful clear report did not replace cache: %+v, %d calls", got, calls)
	}
}

func TestWeatherContextUsesOnlyUpcomingOperationWithValidObservation(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	cache := NewWeatherCache(nil)
	for _, station := range []string{"KSFO", "KJFK"} {
		cache.byStation[station] = feeds.Advisory{Airport: station, ObservedAt: now.Add(-time.Minute), EndsAt: new(now.Add(time.Hour)), Reason: "thunderstorm"}
	}
	flight := feeds.Flight{Status: "airborne", OriginICAO: "KSFO", DestinationICAO: "KJFK", ScheduledDeparture: new(now.Add(-20 * time.Minute)), ScheduledArrival: new(now.Add(time.Hour))}
	if reports := cache.ForFlight(flight, now); len(reports) != 1 || reports[0].Airport != "KJFK" || reports[0].Operation != "arrival" {
		t.Fatalf("airborne weather context was not arrival-only: %+v", reports)
	}
	flight.Status = "diverted"
	if reports := cache.ForFlight(flight, now); len(reports) != 0 {
		t.Fatalf("diverted flight returned original route reports: %+v", reports)
	}
	flight.Status = "scheduled"
	flight.ScheduledDeparture, flight.ScheduledArrival = new(now.Add(8*time.Hour)), new(now.Add(12*time.Hour))
	if reports := cache.ForFlight(flight, now); len(reports) != 0 {
		t.Fatalf("report was shown for a distant future operation: %+v", reports)
	}
}
