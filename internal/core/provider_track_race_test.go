package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

type providerTrackRaceSource struct {
	feeds.FlightSource
	flight feeds.Flight
	track  func(context.Context, string) ([]feeds.Position, error)
}

func (s providerTrackRaceSource) Get(context.Context, string) (feeds.Flight, error) {
	return s.flight, nil
}

func (s providerTrackRaceSource) Track(ctx context.Context, id string) ([]feeds.Position, error) {
	return s.track(ctx, id)
}

func TestPeriodicProviderTrackReconcilesFlightCallbacks(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"same movement", "new"},
		{"takeoff correction", ""},
		{"schedule correction", ""},
		{"destination correction", ""},
		{"assignment changed and restored", ""},
		{"tail withdrawn and restored", "old"},
		{"takeoff withdrawn and restored", ""},
		{"partial response error", "old"},
		{"last tracker removed", "untracked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store, err := Open(filepath.Join(t.TempDir(), "radar.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			now := time.Now().UTC().Truncate(time.Second)
			takeoff, schedule, arrival := now.Add(-time.Hour), now.Add(-70*time.Minute), now.Add(time.Hour)
			flight := feeds.Flight{
				ID: "UAL123", Number: "UA123", Source: "flightaware_aeroapi", Status: "airborne",
				Origin: "SFO", OriginICAO: "KSFO", Destination: "EWR", DestinationICAO: "KEWR",
				TailNumber: "N123AA", ScheduledDeparture: &schedule, ActualTakeoff: &takeoff, ScheduledArrival: &arrival,
				ArrivalGate: "A1", ObservedAt: now.Add(-30 * time.Second),
			}
			oldFix := feeds.Position{At: now.Add(-time.Minute), Latitude: 40, Longitude: -105, Source: flight.Source}
			newFix := feeds.Position{At: now.Add(-time.Second), Latitude: 41, Longitude: -104, Source: flight.Source}
			if err := store.Observe(ctx, flight, []feeds.Position{oldFix}, nil, nil); err != nil {
				t.Fatal(err)
			}
			owner := friendInstallation(t, store)
			if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
				t.Fatal(err)
			}
			polled, callback := flight, flight
			polled.ObservedAt = now.Add(-20 * time.Second)
			callback.ObservedAt, callback.ArrivalGate = now.Add(-10*time.Second), "B2"
			source := providerTrackRaceSource{flight: polled, track: func(ctx context.Context, id string) ([]feeds.Position, error) {
				if id != flight.ID {
					t.Errorf("track request ID = %q, want %q", id, flight.ID)
				}
				switch tc.name {
				case "takeoff correction":
					callback.ActualTakeoff = new(takeoff.Add(10 * time.Minute))
				case "schedule correction":
					callback.ScheduledDeparture = new(schedule.Add(10 * time.Minute))
				case "destination correction":
					callback.Destination, callback.DestinationICAO = "JFK", "KJFK"
				case "assignment changed and restored", "tail withdrawn and restored", "takeoff withdrawn and restored":
					changed := callback
					switch tc.name {
					case "assignment changed and restored":
						changed.TailNumber = "N999AA"
					case "tail withdrawn and restored":
						changed.TailNumber = ""
					case "takeoff withdrawn and restored":
						changed.ActualTakeoff = nil
					}
					changed.ObservedAt = callback.ObservedAt.Add(-time.Second)
					if err := store.IngestAeroObservation(ctx, changed, now); err != nil {
						t.Errorf("intervening assignment: %v", err)
						return nil, err
					}
				case "last tracker removed":
					if err := store.Untrack(ctx, owner, id); err != nil {
						t.Errorf("remove tracker: %v", err)
						return nil, err
					}
					return []feeds.Position{newFix}, nil
				}
				if err := store.IngestAeroObservation(ctx, callback, now); err != nil {
					t.Errorf("flight callback: %v", err)
					return nil, err
				}
				if tc.name == "partial response error" {
					return []feeds.Position{newFix}, feeds.ErrUnavailable
				}
				return []feeds.Position{newFix}, nil
			}}
			refresher := Refresher{Store: store, Flights: source, Logger: slog.New(slog.DiscardHandler)}
			if err := refresher.refresh(ctx); err != nil {
				t.Fatal(err)
			}
			detail, err := store.DetailsForInstallation(ctx, owner, flight.ID)
			if err != nil {
				t.Fatal(err)
			}
			positions, revision, err := store.RadarProviderPositions(ctx, owner, flight.ID, nil, &flight.ObservedAt, 256)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "untracked" {
				if detail.Tracked || len(positions) != 0 || revision != nil ||
					len(detail.Telemetry) != 1 || !detail.Telemetry[0].At.Equal(oldFix.At) {
					t.Fatalf("untracked poll changed cached scope: detail=%+v, page=%+v, revision=%v", detail, positions, revision)
				}
				return
			}
			if detail.Flight.ArrivalGate != callback.ArrivalGate || !detail.Flight.ObservedAt.Equal(callback.ObservedAt) {
				t.Fatalf("radar poll rewrote the newer flight callback: %+v", detail.Flight)
			}
			switch tc.want {
			case "new":
				if revision == nil || !revision.After(callback.ObservedAt) || len(positions) != 1 || !positions[0].At.Equal(newFix.At) ||
					len(detail.Telemetry) != 1 || !detail.Telemetry[0].At.Equal(newFix.At) {
					t.Fatalf("provider track did not advance independently: detail=%+v, page=%+v, revision=%v", detail.Telemetry, positions, revision)
				}
				if same, _, err := store.RadarProviderPositions(ctx, owner, flight.ID, nil, revision, 256); err != nil || len(same) != 0 {
					t.Fatalf("known provider revision returned new fixes: %+v, %v", same, err)
				}
				olderFetch := aeroObservationScope{providerTrackFetched: now.Add(-5 * time.Second)}
				if err := store.observe(ctx, polled, []feeds.Position{oldFix}, nil, nil, observationPoll, olderFetch); err != nil {
					t.Fatal(err)
				}
				if same, current, err := store.RadarProviderPositions(ctx, owner, flight.ID, nil, revision, 256); err != nil || current == nil || !current.Equal(*revision) || len(same) != 0 {
					t.Fatalf("older fetch replaced the newer track: %+v, %v, %v", same, current, err)
				}
				fresh := callback
				fresh.ObservedAt = callback.ObservedAt.Add(time.Nanosecond)
				if err := store.Observe(ctx, fresh, []feeds.Position{oldFix}, nil, nil); err != nil {
					t.Fatal(err)
				}
				updated, next, err := store.RadarProviderPositions(ctx, owner, flight.ID, nil, revision, 256)
				if err != nil || next == nil || !next.After(*revision) || len(updated) != 1 || !updated[0].At.Equal(oldFix.At) {
					t.Fatalf("newer replacement moved the radar revision backwards: %+v, %v, %v", updated, next, err)
				}
			case "old":
				if len(positions) != 1 || !positions[0].At.Equal(oldFix.At) {
					t.Fatalf("saved telemetry was replaced by an invalid poll: %+v", positions)
				}
			default:
				if len(positions) != 0 || len(detail.Telemetry) != 0 {
					t.Fatalf("obsolete scope kept provider telemetry: detail=%+v, page=%+v", detail.Telemetry, positions)
				}
			}
		})
	}
}
