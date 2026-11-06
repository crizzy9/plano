package core

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

func TestCaptureDelaySamplesWithObservedLabel(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := OpenWithOptions(path, StoreOptions{CollectDelaySamples: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	depart := time.Date(2026, 11, 12, 19, 30, 0, 0, time.UTC)
	eta := depart.Add(10 * time.Minute)
	flight := feeds.Flight{ID: "test-provider-instance", Number: "XY123", Origin: "JFK", Destination: "LHR", Status: "scheduled", ObservedAt: depart.Add(-time.Hour), ScheduledDeparture: &depart, EstimatedDeparture: &eta}
	ordinary := flight
	ordinary.ObservedAt = ordinary.ObservedAt.Add(-time.Minute)
	ordinary.EstimatedDeparture = nil
	if err := store.Observe(ctx, ordinary, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.observeTracked(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ExportDelaySamples(ctx, path, &output); err != nil || output.Len() != 0 {
		t.Fatalf("incomplete example exported: %v %s", err, output.String())
	}
	actual := depart.Add(20 * time.Minute)
	labelTime := actual.Add(2 * time.Minute)
	flight.Status, flight.ObservedAt, flight.ActualDeparture = "taxi_out", labelTime, &actual
	if err := store.observeTracked(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := ExportDelaySamples(ctx, path, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, flight.ID) || strings.Contains(text, flight.Number) {
		t.Fatal("export exposes provider flight identity")
	}
	rows, err := predict.ReadExamples(&output)
	if err != nil || len(rows) != 1 {
		t.Fatalf("invalid export: %v (%s)", err, text)
	}
	if rows[0].Features.AirlineDelayMinutes != 10 || !rows[0].DepartedAt.Equal(actual) || rows[0].LabelObservedAt == nil || !rows[0].LabelObservedAt.Equal(labelTime) {
		t.Fatalf("unexpected example %#v", rows[0])
	}
	read := func() predict.Example {
		t.Helper()
		output.Reset()
		if err := ExportDelaySamples(ctx, path, &output); err != nil {
			t.Fatal(err)
		}
		rows, err := predict.ReadExamples(&output)
		if err != nil || len(rows) != 1 {
			t.Fatalf("invalid export: %+v %v", rows, err)
		}
		return rows[0]
	}
	rescheduled := depart.Add(90 * time.Minute)
	flight.ScheduledDeparture, flight.ObservedAt, flight.ActualDeparture = &rescheduled, labelTime.Add(time.Minute), nil
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(); !got.DepartedAt.Equal(actual) || got.LabelObservedAt == nil || !got.LabelObservedAt.Equal(labelTime) {
		t.Fatalf("missing gate-out changed the label: %+v", got)
	}
	corrected := actual.Add(time.Minute)
	flight.ObservedAt, flight.ActualDeparture = labelTime.Add(2*time.Minute), &corrected
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := read()
	if !got.DepartedAt.Equal(corrected) || got.LabelObservedAt == nil || !got.LabelObservedAt.Equal(flight.ObservedAt) ||
		!got.ScheduledDeparture.Equal(depart) || !got.ObservedAt.Equal(depart.Add(-time.Hour)) || got.Features.AirlineDelayMinutes != 10 {
		t.Fatalf("correction changed the original forecast or lost its revised outcome: %+v", got)
	}
}

func TestAeroCallbackCompletesAnExistingDelaySample(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := OpenWithOptions(path, StoreOptions{CollectDelaySamples: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	departure := time.Date(2026, 11, 12, 19, 30, 0, 0, time.UTC)
	flight := feeds.Flight{ID: "XY123-abc", Number: "XY123", Source: "flightaware_aeroapi", Status: "scheduled",
		ObservedAt: departure.Add(-time.Hour), ScheduledDeparture: &departure}
	if err := store.observeTracked(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
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
	actual := departure.Add(10 * time.Minute)
	label := actual.Add(time.Minute)
	flight.Status, flight.ActualDeparture, flight.ObservedAt = "taxi_out", &actual, label
	if err := store.observe(ctx, flight, nil, nil, nil, false, flight.ID); err != nil {
		t.Fatal(err)
	}
	flight.ObservedAt = label.Add(time.Minute)
	if err := store.observeTracked(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ExportDelaySamples(ctx, path, &output); err != nil {
		t.Fatal(err)
	}
	rows, err := predict.ReadExamples(&output)
	if err != nil || len(rows) != 1 || rows[0].LabelObservedAt == nil || !rows[0].LabelObservedAt.Equal(label) {
		t.Fatalf("callback label was not kept: %+v %v", rows, err)
	}
	flight.Status, flight.ActualDeparture, flight.ObservedAt = "cancelled", nil, label.Add(2*time.Minute)
	if err := store.observe(ctx, flight, nil, nil, nil, false, flight.ID); err != nil {
		t.Fatal(err)
	}
	var cleared int
	if err := store.db.QueryRowContext(ctx, `SELECT departed_ns IS NULL AND label_observed_ns IS NULL FROM delay_samples WHERE flight_id=?`, flight.ID).Scan(&cleared); err != nil || cleared != 1 {
		t.Fatalf("cancellation did not clear the captured outcome: %d %v", cleared, err)
	}
	output.Reset()
	if err := ExportDelaySamples(ctx, path, &output); err != nil || output.Len() != 0 {
		t.Fatalf("cancelled sample exported: %v %s", err, output.String())
	}
}

func TestDelaySampleClearsInvalidGateOutCorrections(t *testing.T) {
	schedule := time.Date(2026, 11, 12, 19, 30, 0, 0, time.UTC)
	forecastAt := schedule.Add(-time.Hour)
	for _, tc := range []struct {
		name     string
		actual   time.Time
		observed time.Time
	}{
		{"at forecast", forecastAt, schedule.Add(time.Hour)},
		{"late relative to original schedule", schedule.Add(25 * time.Hour), schedule.Add(26 * time.Hour)},
		{"after observation", schedule.Add(2 * time.Hour), schedule.Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "test.db")
			store, err := OpenWithOptions(path, StoreOptions{CollectDelaySamples: true})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			flight := feeds.Flight{ID: "XY123-correction", Number: "XY123", Status: "scheduled", ObservedAt: forecastAt, ScheduledDeparture: &schedule}
			if err := store.observeTracked(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			actual := schedule.Add(10 * time.Minute)
			flight.Status, flight.ObservedAt, flight.ActualDeparture = "taxi_out", actual.Add(time.Minute), &actual
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			flight.ScheduledDeparture, flight.ObservedAt, flight.ActualDeparture = &tc.actual, tc.observed, &tc.actual
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			var cleared int
			if err := store.db.QueryRowContext(ctx, `SELECT departed_ns IS NULL AND label_observed_ns IS NULL FROM delay_samples WHERE flight_id=?`, flight.ID).Scan(&cleared); err != nil || cleared != 1 {
				t.Fatalf("correction did not clear the captured outcome: %d %v", cleared, err)
			}
			var output bytes.Buffer
			if err := ExportDelaySamples(ctx, path, &output); err != nil || output.Len() != 0 {
				t.Fatalf("invalid sample exported: %v %s", err, output.String())
			}
			flight.ObservedAt, flight.ActualDeparture = tc.observed.Add(time.Minute), &actual
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := ExportDelaySamples(ctx, path, &output); err != nil {
				t.Fatal(err)
			}
			rows, err := predict.ReadExamples(&output)
			if err != nil || len(rows) != 1 || !rows[0].ScheduledDeparture.Equal(schedule) || !rows[0].DepartedAt.Equal(actual) ||
				rows[0].LabelObservedAt == nil || !rows[0].LabelObservedAt.Equal(flight.ObservedAt) {
				t.Fatalf("valid correction did not relabel the original forecast: %+v %v", rows, err)
			}
		})
	}
}

func TestExportDelaySamplesSkipsLegacyContradictedLabels(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := OpenWithOptions(path, StoreOptions{CollectDelaySamples: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	schedule := time.Date(2026, 11, 12, 19, 30, 0, 0, time.UTC)
	actual := schedule.Add(10 * time.Minute)
	for _, id := range []string{"cancelled-fixture", "corrected-fixture", "missing-fixture"} {
		flight := feeds.Flight{ID: id, Status: "scheduled", ObservedAt: schedule.Add(-time.Hour), ScheduledDeparture: &schedule}
		if err := store.observeTracked(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		flight.Status, flight.ObservedAt, flight.ActualDeparture = "taxi_out", actual.Add(time.Minute), &actual
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate observations stored by an older collector or with capture disabled.
	store.collectDelaySamples = false
	for _, flight := range []feeds.Flight{
		{ID: "cancelled-fixture", Status: "cancelled"},
		{ID: "corrected-fixture", Status: "taxi_out", ActualDeparture: new(actual.Add(time.Second))},
		{ID: "missing-fixture", Status: "taxi_out"},
	} {
		flight.ObservedAt = actual.Add(2 * time.Minute)
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := ExportDelaySamples(ctx, path, &output); err != nil {
		t.Fatal(err)
	}
	rows, err := predict.ReadExamples(&output)
	if err != nil || len(rows) != 1 || rows[0].FlightID != hash("delay-sample\x00missing-fixture") || !rows[0].DepartedAt.Equal(actual) {
		t.Fatalf("unexpected labels after legacy reconciliation: %+v %v", rows, err)
	}
}

func TestGateHistoryUsesSameRouteWithObservedOperation(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 11, 12, 19, 30, 0, 0, time.UTC)
	flight := feeds.Flight{ID: "upcoming", Number: "UA123", Origin: "SFO", Destination: "EWR", Status: "scheduled"}
	for i, gate := range []string{"F12", "F12", "F12", "F13"} {
		past := flight
		actual := now.AddDate(0, 0, -i-1)
		past.ID, past.Status, past.DepartureGate, past.ActualDeparture, past.ObservedAt = gate+actual.Format(time.DateOnly), "arrived", gate, &actual, actual.Add(time.Hour)
		if err := store.Observe(ctx, past, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := store.PredictGate(ctx, flight, false, now)
	if err != nil || gate == nil || gate.Gate != "F12" || gate.Samples != 4 || gate.Share != .75 {
		t.Fatalf("prediction %v (%v)", gate, err)
	}
	flight.DepartureGate = "F17"
	gate, err = store.PredictGate(ctx, flight, false, now)
	if err != nil || gate != nil {
		t.Fatalf("assigned gate replaced: %v (%v)", gate, err)
	}
}

func TestGateHistoryUsesTheReportedTerminalForEachDirection(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "gates.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	flight := feeds.Flight{ID: "upcoming", Number: "UA123", Origin: "SFO", Destination: "EWR", Status: "scheduled",
		DepartureTerminal: "C", ArrivalTerminal: "d"}
	for i := range 9 {
		past := flight
		actual := now.AddDate(0, 0, -i-1)
		past.ID, past.Status, past.ObservedAt = actual.Format(time.DateOnly), "arrived", actual
		past.ActualDeparture, past.ActualArrival = new(actual.Add(-6*time.Hour)), &actual
		past.DepartureTerminal, past.DepartureGate = "A", "A12"
		past.ArrivalTerminal, past.ArrivalGate = "B", "B7"
		switch {
		case i < 3:
			past.DepartureTerminal, past.DepartureGate = " c ", "C12"
		case i < 6:
			past.ArrivalTerminal, past.ArrivalGate = " D ", "D22"
		default:
			past.DepartureTerminal, past.ArrivalTerminal = "", ""
		}
		if err := store.Observe(ctx, past, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		arrival bool
		gate    string
	}{{false, "C12"}, {true, "D22"}} {
		got, err := store.PredictGate(ctx, flight, tc.arrival, now)
		if err != nil || got == nil || got.Gate != tc.gate || got.Samples != 3 || got.Share != 1 {
			t.Fatalf("arrival=%v: prediction = %+v, error = %v", tc.arrival, got, err)
		}
	}
	flight.ArrivalTerminal = "E"
	if got, err := store.PredictGate(ctx, flight, true, now); err != nil || got != nil {
		t.Fatalf("prediction without compatible terminal history = %+v, error = %v", got, err)
	}
}
