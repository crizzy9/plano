package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

func TestDelaySampleHorizonsKeepIndependentForecastsAndCorrectBothLabels(t *testing.T) {
	for _, source := range []string{"flightaware_aeroapi", "fixture"} {
		t.Run(source, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "samples.db")
			store, err := OpenWithOptions(path, StoreOptions{CollectDelaySamples: true})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			scheduled := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Minute)
			flight := feeds.Flight{ID: "UAL123-early-test", Number: "UA123", Source: source,
				Status: "scheduled", ScheduledDeparture: &scheduled, Origin: "EWR", Destination: "SFO"}
			observe := func(lead, estimate time.Duration) {
				t.Helper()
				flight.ObservedAt = scheduled.Add(-lead)
				flight.EstimatedDeparture = new(scheduled.Add(estimate))
				if err := store.observeTracked(ctx, flight, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			observe(25*time.Hour+time.Second, 5*time.Minute)
			observe(25*time.Hour, 10*time.Minute)
			observe(6*time.Hour, 15*time.Minute)
			observe(2*time.Hour, 25*time.Minute)
			observe(time.Hour, 30*time.Minute)

			label := func(at time.Time, outcome *time.Time, status string) {
				t.Helper()
				flight.ObservedAt, flight.ActualDeparture, flight.Status = at, outcome, status
				if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			read := func(export func(context.Context, string, io.Writer) error) []predict.Example {
				t.Helper()
				var data bytes.Buffer
				if err := export(ctx, path, &data); err != nil {
					t.Fatal(err)
				}
				rows, err := predict.ReadExamples(&data)
				if err != nil {
					t.Fatal(err)
				}
				return rows
			}
			assertSamples := func(actual, reportedAt time.Time) {
				t.Helper()
				early, standard := read(ExportEarlyDelaySamples), read(ExportDelaySamples)
				if len(early) != 1 || len(standard) != 1 {
					t.Fatalf("exports returned %d early and %d standard samples", len(early), len(standard))
				}
				if early[0].FlightID != standard[0].FlightID || early[0].FlightID == flight.ID ||
					!early[0].ObservedAt.Equal(scheduled.Add(-25*time.Hour)) || !standard[0].ObservedAt.Equal(scheduled.Add(-2*time.Hour)) ||
					early[0].Features.AirlineDelayMinutes != 10 || standard[0].Features.AirlineDelayMinutes != 25 {
					t.Fatalf("captured wrong forecast or identity: early=%+v standard=%+v", early, standard)
				}
				for _, row := range []predict.Example{early[0], standard[0]} {
					if !row.ScheduledDeparture.Equal(scheduled) || !row.DepartedAt.Equal(actual) || row.LabelObservedAt == nil || !row.LabelObservedAt.Equal(reportedAt) {
						t.Fatalf("invalid label: %+v", row)
					}
				}
			}
			if early, standard := read(ExportEarlyDelaySamples), read(ExportDelaySamples); len(early)+len(standard) != 0 {
				t.Fatalf("unlabeled forecasts exported: early=%v standard=%v", early, standard)
			}
			actual := scheduled.Add(40 * time.Minute)
			reportedAt := actual.Add(time.Minute)
			label(reportedAt, &actual, "taxi_out")
			assertSamples(actual, reportedAt)
			corrected := actual.Add(time.Minute)
			reportedAt = corrected.Add(time.Minute)
			label(reportedAt, &corrected, "taxi_out")
			assertSamples(corrected, reportedAt)
			label(reportedAt.Add(time.Minute), nil, "cancelled")
			if early, standard := read(ExportEarlyDelaySamples), read(ExportDelaySamples); len(early)+len(standard) != 0 {
				t.Fatalf("cancelled labels survived: early=%v standard=%v", early, standard)
			}
		})
	}
}

func TestDelayModelSelectionUsesTheDepartureHorizon(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	fit := func(lead, delay time.Duration) *predict.Model {
		t.Helper()
		rows := make([]predict.Example, 40)
		for i := range rows {
			departure := now.AddDate(0, 0, i-90)
			rows[i] = predict.Example{FlightID: fmt.Sprintf("fixture-%d", i), ObservedAt: departure.Add(-lead),
				ScheduledDeparture: departure, DepartedAt: departure.Add(delay), Features: predict.Features{AirlineDelayMinutes: 20}}
		}
		model, _, err := predict.Fit(rows, predict.Options{HoldoutFraction: 0.2, Ridge: 0.01})
		if err != nil {
			t.Fatal(err)
		}
		return model
	}
	standard, early := fit(2*time.Hour, 15*time.Minute), fit(12*time.Hour, 27*time.Minute)
	store := Store{delayModel: standard, earlyDelayModel: early}
	for _, tc := range []struct {
		lead time.Duration
		want *predict.Model
	}{{12 * time.Hour, early}, {2 * time.Hour, standard}} {
		departure := now.Add(tc.lead)
		flight := feeds.Flight{Status: "scheduled", ObservedAt: now, ScheduledDeparture: &departure,
			EstimatedDeparture: new(departure.Add(20 * time.Minute))}
		result := PredictWithModel(store.delayModelFor(flight, now), flight, nil, nil, now)
		if result == nil || result.ModelVersion != tc.want.ID() || result.Method != "learned_estimate" {
			t.Fatalf("lead %s selected wrong model: %+v", tc.lead, result)
		}
	}
	store.earlyDelayModel = nil
	if got := store.delayModelFor(feeds.Flight{ScheduledDeparture: new(now.Add(12 * time.Hour))}, now); got != standard {
		t.Fatal("missing early artifact changed the original model selection")
	}
}
