package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

type forecastFixture func(context.Context, []string, time.Time) ([]feeds.TAFPeriod, error)

func (f forecastFixture) Forecasts(ctx context.Context, stations []string, now time.Time) ([]feeds.TAFPeriod, error) {
	return f(ctx, stations, now)
}

func TestWeatherForecastCacheReplacesSuccessfulBatches(t *testing.T) {
	ctx := t.Context()
	store, owner := openWeatherForecastFixtureStore(t)
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	flight := feeds.Flight{ID: "tracked-forecast", Number: "UA123", Status: "scheduled", Source: "fixture", ObservedAt: now,
		OriginICAO: "KSFO", DestinationICAO: "KJFK", ScheduledDeparture: new(now.Add(-15 * time.Minute)),
		ScheduledArrival: new(now.Add(7 * time.Hour))}
	outsideHorizon := flight
	outsideHorizon.ID, outsideHorizon.OriginICAO = "distant-forecast", "KLAX"
	outsideHorizon.ScheduledDeparture, outsideHorizon.ScheduledArrival = new(now.Add(25*time.Hour)), nil
	untracked := flight
	untracked.ID, untracked.OriginICAO = "untracked-forecast", "KDEN"
	untracked.ScheduledArrival = nil
	for _, snapshot := range []feeds.Flight{flight, outsideHorizon, untracked} {
		if err := store.Observe(ctx, snapshot, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{flight.ID, outsideHorizon.ID} {
		if err := store.Track(ctx, owner, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	departure := feeds.TAFPeriod{Airport: "KSFO", Conditions: "low visibility", Qualifier: "prevailing", IssuedAt: now,
		StartsAt: now.Add(-time.Minute), EndsAt: now.Add(45 * time.Minute)}
	arrival := feeds.TAFPeriod{Airport: "KJFK", Conditions: "thunderstorms", Qualifier: "temporary", Probability: 30, IssuedAt: now,
		StartsAt: now.Add(6 * time.Hour), EndsAt: now.Add(8 * time.Hour)}
	stray, future := departure, arrival
	stray.Airport, future.IssuedAt = "KDEN", now.Add(time.Hour)
	var calls int
	cache := NewWeatherForecastCache(forecastFixture(func(_ context.Context, stations []string, at time.Time) ([]feeds.TAFPeriod, error) {
		if !slices.Equal(stations, []string{"KJFK", "KSFO"}) {
			t.Fatalf("unexpected station batch %v", stations)
		}
		calls++
		switch calls {
		case 1:
			return []feeds.TAFPeriod{arrival, departure, stray, future, departure}, nil
		case 2:
			return nil, feeds.ErrUnavailable
		case 3:
			amended := departure
			amended.Conditions, amended.IssuedAt, amended.EndsAt = "freezing fog", at, at.Add(time.Minute)
			return []feeds.TAFPeriod{amended}, nil
		default:
			return nil, nil
		}
	}))
	if err := cache.refresh(ctx, store, now); err != nil {
		t.Fatal(err)
	}
	initial := cache.ForFlight(flight, now)
	if len(initial) != 2 || initial[0].Operation != "departure" || initial[0].Conditions != "low visibility" ||
		initial[1].Operation != "arrival" || initial[1].Qualifier != "temporary" || initial[1].Probability != 30 {
		t.Fatalf("unexpected forecasts: %+v", initial)
	}
	if got := cache.ForFlight(untracked, now); len(got) != 0 {
		t.Fatalf("unexpected unrequested forecast: %+v", got)
	}
	if err := cache.refresh(ctx, store, now.Add(weatherForecastCadence)); !errors.Is(err, feeds.ErrUnavailable) {
		t.Fatalf("unexpected outage error %v", err)
	}
	if got := cache.ForFlight(flight, now.Add(weatherForecastCadence)); !slices.Equal(got, initial) {
		t.Fatalf("outage changed current forecasts: %+v", got)
	}
	amendedAt := now.Add(2 * weatherForecastCadence)
	if err := cache.refresh(ctx, store, amendedAt); err != nil {
		t.Fatal(err)
	}
	if got := cache.ForFlight(flight, amendedAt); len(got) != 1 || got[0].Conditions != "freezing fog" || !got[0].IssuedAt.Equal(amendedAt) {
		t.Fatalf("amendment or clear did not replace previous forecasts: %+v", got)
	}
	if got := cache.ForFlight(flight, amendedAt.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("expired forecast remained visible: %+v", got)
	}
	if err := cache.refresh(ctx, store, now.Add(3*weatherForecastCadence)); err != nil {
		t.Fatal(err)
	}
	if len(cache.byStation) != 0 || calls != 4 {
		t.Fatalf("successful empty batch retained forecasts: %+v, %d calls", cache.byStation, calls)
	}
}

func TestWeatherForecastCacheUsesPendingOperationAndHalfOpenWindow(t *testing.T) {
	now := time.Date(2026, 11, 6, 23, 30, 0, 0, time.UTC)
	change := now.Add(45 * time.Minute)
	cache := NewWeatherForecastCache(nil)
	first := feeds.TAFPeriod{Airport: "KSFO", Conditions: "freezing rain", Qualifier: "prevailing", IssuedAt: now,
		StartsAt: now, EndsAt: change}
	second := feeds.TAFPeriod{Airport: "KSFO", Conditions: "low ceiling", Qualifier: "prevailing", IssuedAt: now,
		StartsAt: change, EndsAt: now.Add(2 * time.Hour)}
	cache.byStation["KSFO"] = []feeds.TAFPeriod{first, second}
	flight := feeds.Flight{Status: "scheduled", OriginICAO: "KSFO", DestinationICAO: "KSFO", ScheduledDeparture: new(now),
		ScheduledArrival: &change}
	if got := cache.ForFlight(flight, now); len(got) != 2 || got[0].Operation != "departure" || got[0].Conditions != first.Conditions ||
		got[1].Operation != "arrival" || got[1].Conditions != second.Conditions {
		t.Fatalf("forecast selected the wrong side of a midnight change: %+v", got)
	}
	flight.EstimatedDeparture = &change
	if got := cache.ForFlight(flight, now); len(got) != 2 || got[0].Conditions != second.Conditions {
		t.Fatalf("forecast ignored the estimated departure: %+v", got)
	}
	flight.EstimatedDeparture = new(now.Add(-weatherLookback - time.Nanosecond))
	if got := cache.ForFlight(flight, now); len(got) != 1 || got[0].Operation != "arrival" {
		t.Fatalf("forecast showed an overdue departure outside the lookback: %+v", got)
	}
	flight.EstimatedDeparture, flight.Status = nil, "airborne"
	if got := cache.ForFlight(flight, now); len(got) != 1 || got[0].Operation != "arrival" {
		t.Fatalf("forecast showed a completed departure: %+v", got)
	}
	for _, status := range []string{"diverted", "arrived", "cancelled"} {
		flight.Status = status
		if got := cache.ForFlight(flight, now); len(got) != 0 {
			t.Fatalf("%s flight returned forecast: %+v", status, got)
		}
	}
}

func TestWeatherForecastCacheRotatesFullStationBatches(t *testing.T) {
	ctx := t.Context()
	store, owner := openWeatherForecastFixtureStore(t)
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	var stationList []string
	for i := range weatherForecastBatchSize + 1 {
		station := fmt.Sprintf("KAA%c", 'A'+i)
		stationList = append(stationList, station)
		flight := feeds.Flight{ID: "flight-" + station, Number: "UA123", Status: "scheduled", Source: "fixture", ObservedAt: now,
			OriginICAO: station, ScheduledDeparture: new(now.Add(8 * time.Hour))}
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	full, remainder := stationList[:weatherForecastBatchSize], stationList[weatherForecastBatchSize:]
	wanted := [][]string{full, remainder, full}
	var batches int
	cache := NewWeatherForecastCache(forecastFixture(func(_ context.Context, got []string, _ time.Time) ([]feeds.TAFPeriod, error) {
		if batches >= len(wanted) || !slices.Equal(got, wanted[batches]) {
			t.Fatalf("unexpected forecast batch %d: %v", batches, got)
		}
		batches++
		return nil, nil
	}))
	for i := range wanted {
		if err := cache.refresh(ctx, store, now.Add(time.Duration(i)*weatherForecastCadence)); err != nil {
			t.Fatal(err)
		}
	}
	if batches != len(wanted) {
		t.Fatalf("got %d forecast batches, want %d", batches, len(wanted))
	}
}

func openWeatherForecastFixtureStore(t *testing.T) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "weather-forecast.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	token, err := store.CreateInstallation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	return store, owner
}
