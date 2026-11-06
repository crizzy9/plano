package core

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestGateHistoryCountsVerifiedFlightInstancesOnce(t *testing.T) {
	store := openGateHistoryStore(t)
	now := time.Date(2026, 11, 12, 19, 30, 0, 500_000_000, time.UTC)
	actual := now.Add(-24 * time.Hour)
	flight := feeds.Flight{ID: "UAL123-target", Number: "UA123", Origin: "SFO", Destination: "EWR", Status: "scheduled"}

	for _, tc := range []struct {
		id, source, provider, gate string
		observed                   time.Time
	}{
		{"UAL123-target", "flightaware_aeroapi", "", "F99", actual},
		{"schedule-target", "flightaware_schedule", flight.ID, "F99", actual},
		{"UAL123-past", "flightaware_aeroapi", "", "F99", actual},
		{"schedule-past", "flightaware_schedule", "UAL123-past", "\t f12 \n", actual.Add(time.Minute)},
		{"local-collision", "fixture", "", "F12", actual},
		{"other-schedule", "flightaware_schedule", "local-collision", "F13", actual},
		{"schedule-future-observation", "flightaware_schedule", "UAL123-past", "F99", now.Add(time.Second)},
	} {
		past := flight
		past.ID, past.Source, past.Status = tc.id, tc.source, "arrived"
		past.DepartureGate, past.ActualDeparture, past.ObservedAt = tc.gate, &actual, tc.observed
		observeGateHistory(t, store, past, tc.provider)
	}
	// Some saved canonical rows predate the verified instance index.
	if _, err := store.db.ExecContext(t.Context(), `DELETE FROM aero_instance_keys WHERE flight_id IN (?,?)`, flight.ID, "UAL123-past"); err != nil {
		t.Fatal(err)
	}
	past := flight
	past.ID, past.Status, past.DepartureGate = "within-current-second", "taxi_out", "F12"
	past.ObservedAt, past.ActualDeparture = now, new(now.Add(-100*time.Millisecond))
	observeGateHistory(t, store, past, "")

	got, err := store.PredictGate(t.Context(), flight, false, now)
	if err != nil || got == nil || got.Gate != "F12" || got.Samples != 4 || got.Share != .75 {
		t.Fatalf("gate prediction = %+v, error = %v", got, err)
	}

	flight.ID = "schedule-target"
	got, err = store.PredictGate(t.Context(), flight, false, now)
	if err != nil || got == nil || got.Gate != "F12" || got.Samples != 4 || got.Share != .75 {
		t.Fatalf("gate prediction for target alias = %+v, error = %v", got, err)
	}
}

func TestGateHistoryUsesTerminalOfLatestQualifiedAlias(t *testing.T) {
	store := openGateHistoryStore(t)
	now := time.Date(2026, 11, 12, 19, 30, 0, 0, time.UTC)
	actual := now.Add(-24 * time.Hour)
	flight := feeds.Flight{ID: "upcoming", Number: "UA123", Origin: "SFO", Destination: "EWR", Status: "scheduled", ArrivalTerminal: " C "}

	for i := range 3 {
		past := flight
		past.ID = actual.AddDate(0, 0, -i).Format(time.DateOnly)
		past.Status, past.ObservedAt, past.ActualArrival = "arrived", actual, &actual
		past.ArrivalTerminal, past.ArrivalGate = "C", "C12"
		observeGateHistory(t, store, past, "")
	}
	for _, tc := range []struct {
		provider, latestID, latestSource, status, terminal, gate string
		actual                                                   *time.Time
		destination, number                                      string
	}{
		{"UAL123-moved", "UAL123-moved", "flightaware_aeroapi", "arrived", "D", "D12", &actual, "", ""},
		{"UAL123-corrected", "UAL123-corrected", "flightaware_aeroapi", "scheduled", "D", "", nil, "", ""},
		{"UAL123-cancelled", "UAL123-cancelled", "flightaware_aeroapi", "cancelled", "", "", nil, "", ""},
		{"UAL123-no-gate", "UAL123-no-gate", "flightaware_aeroapi", "arrived", "C", "", &actual, "", ""},
		{"UAL123-unconfirmed", "schedule-unconfirmed", "flightaware_schedule", "scheduled", "D", "", nil, "", ""},
		{"UAL123-diverted", "UAL123-diverted", "flightaware_aeroapi", "diverted", "", "", nil, "JFK", ""},
		{"UAL123-rerouted", "UAL123-rerouted", "flightaware_aeroapi", "arrived", "C", "C13", nil, "JFK", ""},
		{"UAL123-gate-change", "UAL123-gate-change", "flightaware_aeroapi", "arrived", "C", "C15", nil, "", ""},
		{"UAL123-codeshare", "UAL123-codeshare", "flightaware_aeroapi", "arrived", "C", "C13", nil, "", "AC42"},
	} {
		past := flight
		past.ID, past.Source, past.Status = "older-"+tc.provider, "flightaware_schedule", "arrived"
		past.ObservedAt, past.ActualArrival, past.ArrivalTerminal, past.ArrivalGate = actual, &actual, "C", "C13"
		observeGateHistory(t, store, past, tc.provider)
		past.ID, past.Source, past.Status = tc.latestID, tc.latestSource, tc.status
		past.ObservedAt, past.ActualArrival, past.ArrivalTerminal, past.ArrivalGate = actual.Add(time.Minute), tc.actual, tc.terminal, tc.gate
		if tc.destination != "" {
			past.Destination = tc.destination
		}
		if tc.number != "" {
			past.Number = tc.number
		}
		provider := tc.provider
		if tc.latestID == tc.provider {
			provider = "" // Observe indexes canonical AeroAPI rows.
		}
		observeGateHistory(t, store, past, provider)
	}
	// The cross-route correction must also work for an unindexed canonical row.
	if _, err := store.db.ExecContext(t.Context(), `DELETE FROM aero_instance_keys WHERE flight_id=?`, "UAL123-diverted"); err != nil {
		t.Fatal(err)
	}

	got, err := store.PredictGate(t.Context(), flight, true, now)
	if err != nil || got == nil || got.Gate != "C12" || got.Samples != 5 || got.Share != .60 {
		t.Fatalf("gate prediction = %+v, error = %v", got, err)
	}
	flight.Status = "diverted"
	if got, err := store.PredictGate(t.Context(), flight, true, now); err != nil || got != nil {
		t.Fatalf("diverted flight received a predicted arrival gate: %+v, %v", got, err)
	}
}

func openGateHistoryStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gates.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func observeGateHistory(t *testing.T, store *Store, flight feeds.Flight, provider string) {
	t.Helper()
	if err := store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if provider != "" {
		if _, err := store.db.ExecContext(t.Context(), `INSERT INTO aero_instance_keys VALUES(?,?)`, provider, flight.ID); err != nil {
			t.Fatal(err)
		}
	}
}
