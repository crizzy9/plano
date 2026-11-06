package core

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestEstimateAlertsAccumulateAcrossRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "estimates.db")
	open := func() *Store {
		t.Helper()
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	store := open()
	now := time.Now().UTC()
	departure, arrival := now.Add(30*time.Hour), now.Add(36*time.Hour)
	departureReference, arrivalReference := departure.Add(10*time.Minute), arrival.Add(-10*time.Minute)
	flight := feeds.Flight{
		ID: "cumulative-estimates", Number: "AA2", Origin: "JFK", Destination: "SFO", Status: "scheduled",
		ScheduledDeparture: &departure, ScheduledArrival: &arrival, EstimatedDeparture: &departureReference, EstimatedArrival: &arrivalReference,
		ObservedAt: now,
	}
	observeFlightAlertFixture(t, store, &flight)
	token, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"ios", "android"} {
		if err := store.Register(ctx, owner, Device{Platform: platform, Kind: "alert", Token: platform + "-estimate-device"}, now); err != nil {
			t.Fatal(err)
		}
	}
	// Reopen with the pre-upgrade schema and an existing estimate snapshot.
	if _, err := store.db.ExecContext(ctx, `DROP TABLE flight_estimate_anchors`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = open()
	move := func(minutes int) {
		t.Helper()
		shift := time.Duration(minutes) * time.Minute
		flight.EstimatedDeparture, flight.EstimatedArrival = new(departureReference.Add(shift)), new(arrivalReference.Add(-shift))
		observeFlightAlertFixture(t, store, &flight)
	}
	move(2)
	claimFlightAlertJobs(t, store, 0)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = open()
	move(4)
	claimFlightAlertJobs(t, store, 0)
	move(6)
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 4),
		"Departure estimate changed|AA2 · 6 min later", "Arrival estimate changed|AA2 · 6 min earlier")
	move(8)
	move(4)
	move(2)
	claimFlightAlertJobs(t, store, 0)
	move(0)
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 4),
		"Departure estimate changed|AA2 · 6 min earlier", "Arrival estimate changed|AA2 · 6 min later")
	assertFlightAlertHistory(t, store, flight.ID,
		"etd|AA2 · 6 min later", "eta|AA2 · 6 min earlier", "etd|AA2 · 6 min earlier", "eta|AA2 · 6 min later")
}

func TestEstimateAlertResetsWhenEstimateIsWithdrawn(t *testing.T) {
	store, flight := flightAlertDispatchFixture(t)
	arrival := flight.ObservedAt.Add(35 * time.Hour)
	flight.ScheduledArrival = &arrival
	moveArrival := func(minutes int) {
		t.Helper()
		flight.EstimatedArrival = new(arrival.Add(time.Duration(minutes) * time.Minute))
		observeFlightAlertFixture(t, store, &flight)
	}
	moveArrival(10)
	stale := claimFlightAlertJobs(t, store, 2)
	flight.EstimatedArrival = nil
	observeFlightAlertFixture(t, store, &flight)
	assertFlightAlertSends(t, store, stale)
	moveArrival(2)
	moveArrival(4)
	claimFlightAlertJobs(t, store, 0)
	moveArrival(6)
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2), "Arrival estimate changed|AA2 · 6 min later")
	flight.Status = "arrived"
	moveArrival(12)
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2), "Arrived at the gate|AA2")
	assertFlightAlertHistory(t, store, flight.ID, "eta|AA2 · 10 min later", "eta|AA2 · 6 min later", "arrived|AA2", "eta|AA2 · 6 min later")
}
