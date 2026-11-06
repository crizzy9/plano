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
