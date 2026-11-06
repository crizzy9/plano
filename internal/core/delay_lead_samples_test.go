package core

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

func TestDelaySampleTargetsSpreadLeadTimesAcrossFlights(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "delay-leads.db")
	store, err := OpenWithOptions(path, StoreOptions{CollectDelaySamples: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	scheduled := time.Date(2026, 11, 12, 19, 30, 0, 0, time.UTC)
	departed := scheduled.Add(20 * time.Minute)
	labelAt := departed.Add(time.Minute)
	cohorts := []struct {
		tables   delaySampleTables
		min, max time.Duration
		export   func(context.Context, string, io.Writer) error
	}{
		{earlyDelaySampleTables, 125 * time.Minute, 25 * time.Hour, ExportEarlyDelaySamples},
		{standardDelaySampleTables, 5 * time.Minute, 2 * time.Hour, ExportDelaySamples},
	}
	flights := []feeds.Flight{
		{ID: "UAL123-lead-a", Source: "flightaware_aeroapi"},
		{ID: "UAL123-lead-b", Source: "flightaware_aeroapi"},
		{ID: "local-lead-c"},
	}
	for _, flight := range flights {
		flight.Status, flight.ScheduledDeparture = "scheduled", &scheduled
		poll := func(lead time.Duration, delay int) {
			t.Helper()
			flight.ObservedAt = scheduled.Add(-lead)
			flight.EstimatedDeparture = new(scheduled.Add(time.Duration(delay) * time.Minute))
			if err := store.observeTracked(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
		}
		for _, cohort := range cohorts {
			target := delaySampleTarget(flight.ID, cohort.tables)
			if target < cohort.min || target > cohort.max || target%time.Minute != 0 {
				t.Fatalf("target %s outside cohort %s–%s", target, cohort.min, cohort.max)
			}
			if target < cohort.max {
				poll(cohort.max, 5)
				poll(target+time.Second, 10)
				table, key := cohort.tables.local, "flight_id"
				if flight.AeroProviderID() != "" {
					table, key = cohort.tables.provider, "provider_id"
				}
				var captured bool
				err := store.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE "+key+"=?)", flight.ID).Scan(&captured)
				if err != nil || captured {
					t.Fatalf("%s captured %s before its %s target: %t, %v", table, flight.ID, target, captured, err)
				}
			}
			poll(target-30*time.Second, 15)
			poll(target-time.Minute, 25)
		}
		flight.Status, flight.ObservedAt, flight.ActualDeparture = "taxi_out", labelAt, &departed
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, cohort := range cohorts {
		var output bytes.Buffer
		if err := cohort.export(ctx, path, &output); err != nil {
			t.Fatal(err)
		}
		rows, err := predict.ReadExamples(&output)
		if err != nil || len(rows) != len(flights) {
			t.Fatalf("export %s: %d rows, %v", cohort.tables.local, len(rows), err)
		}
		want := make(map[string]time.Duration, len(flights))
		for _, flight := range flights {
			want[hash("delay-sample\x00"+flight.ID)] = delaySampleTarget(flight.ID, cohort.tables) - 30*time.Second
		}
		observedLeads := make(map[time.Duration]bool)
		for _, row := range rows {
			lead := row.ScheduledDeparture.Sub(row.ObservedAt)
			if lead != want[row.FlightID] || row.Features.AirlineDelayMinutes != 15 || !row.DepartedAt.Equal(departed) ||
				row.LabelObservedAt == nil || !row.LabelObservedAt.Equal(labelAt) {
				t.Fatalf("incorrect forecast or label in %s export: %+v", cohort.tables.local, row)
			}
			delete(want, row.FlightID)
			observedLeads[lead] = true
		}
		if len(observedLeads) < 2 {
			t.Fatalf("all %s captures used the same lead", cohort.tables.local)
		}
	}
}
