package core

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestAircraftMetadataRetentionRequiresSameProviderAndTail(t *testing.T) {
	source := feeds.NewAeroAPI("fixture-key", &http.Client{Transport: aeroCallbackTransport(func(req *http.Request) (*http.Response, error) {
		id := strings.TrimPrefix(req.URL.Path, "/aeroapi/flights/")
		body := fmt.Sprintf(`{"flights":[{"fa_flight_id":%q,"ident_iata":"UA7","registration":"N12AA","aircraft_type":"B739","status":"Arrived"}]}`, id)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	for _, tc := range []struct {
		name, provider, tail, source string
		wantRetention                bool
	}{
		{"same provider and tail", "UAL7-first", "N12AA", "flightaware_aeroapi", true},
		{"changed provider", "UAL7-second", "N12AA", "flightaware_aeroapi", false},
		{"changed tail", "UAL7-first", "N13AA", "flightaware_aeroapi", false},
		{"withdrawn tail", "UAL7-first", "", "flightaware_aeroapi", false},
		{"unverified provider", "UAL7-first", "N12AA", "flightaware_schedule", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := t.Context()
			prior, err := source.Get(ctx, "UAL7-first")
			if err != nil {
				t.Fatal(err)
			}
			prior.ID = "scheduled_fixture"
			prior.Status = "airborne"
			prior.AircraftAge, prior.AircraftModel = new(16), "BOEING 737-900"
			if err := store.Observe(ctx, prior, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			next, err := source.Get(ctx, tc.provider)
			if err != nil {
				t.Fatal(err)
			}
			next.ID, next.ObservedAt, next.TailNumber, next.Source = prior.ID, prior.ObservedAt.Add(time.Second), tc.tail, tc.source
			if err := store.Observe(ctx, next, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			got, err := store.Flight(ctx, prior.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.AircraftType != "B739" || (got.AircraftAge != nil) != tc.wantRetention || (got.AircraftModel != "") != tc.wantRetention {
				t.Fatalf("stored aircraft = type %q, model %q, age %v", got.AircraftType, got.AircraftModel, got.AircraftAge)
			}
			if tc.wantRetention && (*got.AircraftAge != 16 || got.AircraftModel != prior.AircraftModel) {
				t.Fatalf("metadata changed: age %d, model %q", *got.AircraftAge, got.AircraftModel)
			}
		})
	}
}

func TestDirectInboundRetainsAircraftMetadataOnArrival(t *testing.T) {
	for _, path := range []string{"callback", "poll"} {
		t.Run(path, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "inbound-metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := t.Context()
			now := time.Now().UTC()
			onward := feeds.Flight{ID: "onward", Source: "flightaware_aeroapi", Status: "scheduled", InboundID: "inbound",
				Origin: "JFK", TailNumber: "N12AA", ScheduledDeparture: new(now.Add(time.Hour)), ObservedAt: now.Add(-time.Minute)}
			inbound := feeds.Flight{ID: onward.InboundID, Source: onward.Source, Status: "airborne", Origin: "SFO", Destination: onward.Origin,
				TailNumber: onward.TailNumber, AircraftType: "B739", AircraftAge: new(16), AircraftModel: "BOEING 737-900", ObservedAt: now.Add(-time.Minute)}
			if err := store.Observe(ctx, onward, nil, []feeds.Flight{inbound}, nil); err != nil {
				t.Fatal(err)
			}
			trackAeroCallbackFixture(t, store, onward.ID)
			if path == "callback" {
				other, bare := onward, inbound
				other.ID, bare.AircraftAge, bare.AircraftModel = "onward-without-metadata", nil, ""
				if err := store.Observe(ctx, other, nil, []feeds.Flight{bare}, nil); err != nil {
					t.Fatal(err)
				}
				trackAeroCallbackFixture(t, store, other.ID)
			}
			apply := func(next feeds.Flight) {
				t.Helper()
				if path == "callback" {
					err = store.applyAeroInbound(ctx, inbound.ID, next, next.ObservedAt)
				} else {
					onward.ObservedAt = next.ObservedAt
					err = store.observeTracked(ctx, onward, nil, []feeds.Flight{next}, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			direct := func(id string) feeds.Flight {
				t.Helper()
				details, err := store.Details(ctx, id)
				if err != nil || len(details.Inbound) != 1 {
					t.Fatalf("direct inbound = %+v, %v", details.Inbound, err)
				}
				return details.Inbound[0]
			}
			arrived := inbound
			arrived.Status, arrived.ObservedAt, arrived.ActualArrival = "arrived", now, new(now)
			arrived.AircraftAge, arrived.AircraftModel = nil, ""
			apply(arrived)
			got := direct(onward.ID)
			if got.Status != "arrived" || got.AircraftType != inbound.AircraftType || got.AircraftModel != inbound.AircraftModel || got.AircraftAge == nil || *got.AircraftAge != *inbound.AircraftAge {
				t.Fatalf("arrived aircraft = %+v", got)
			}
			if path == "callback" {
				got := direct("onward-without-metadata")
				if got.Status != "arrived" || got.AircraftAge != nil || got.AircraftModel != "" {
					t.Fatalf("metadata leaked across callback targets: %+v", got)
				}
			}
			arrived.TailNumber, arrived.ObservedAt = "N13AA", now.Add(time.Second)
			apply(arrived)
			if got := direct(onward.ID); got.TailNumber != arrived.TailNumber || got.AircraftAge != nil || got.AircraftModel != "" {
				t.Fatalf("new aircraft inherited previous metadata: %+v", got)
			}
		})
	}
}

func TestAircraftMetadataRetentionCarriesCanadianSourceWithCopiedData(t *testing.T) {
	previous := feeds.Flight{
		ID: "ACA7-live", Source: "flightaware_aeroapi", TailNumber: "C-FIVM",
		AircraftRegistry: &feeds.RegistryAircraftDetails{Source: feeds.CCARRegistrySource, Age: new(18), Model: "777-333ER"},
	}
	for _, tc := range []struct {
		name string
		tail string
		age  *int
		want bool
	}{
		{name: "copied model with existing age", tail: "C-FIVM", age: new(19), want: true},
		{name: "new aircraft", tail: "C-FNNH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := feeds.Flight{ID: previous.ID, Source: previous.Source, TailNumber: tc.tail, AircraftAge: tc.age}
			if changed := retainAircraftMetadata(&next, previous, ""); changed != tc.want {
				t.Fatalf("metadata retained = %t, want %t", changed, tc.want)
			}
			if tc.want {
				if next.AircraftAge != tc.age || next.AircraftModel != "" || next.AircraftRegistry == nil ||
					next.AircraftRegistry.Source != feeds.CCARRegistrySource || next.AircraftRegistry.Age != nil ||
					next.AircraftRegistry.Model != previous.AircraftRegistry.Model {
					t.Fatalf("wrong retained metadata: %+v", next)
				}
			} else if next.AircraftRegistry != nil || next.AircraftModel != "" || next.AircraftAge != nil {
				t.Fatalf("new assignment inherited previous metadata: %+v", next)
			}
		})
	}
}

func TestRegistryMetadataCarryoverChecksCorrectedDepartureDay(t *testing.T) {
	midnight := time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		offset  time.Duration
		retains bool
	}{
		{"same registration day", 8 * time.Minute, true},
		{"actual departure before registration day", -5 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store, err := Open(filepath.Join(t.TempDir(), "registry-departure.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			prior := feeds.Flight{ID: "ACA7-live", Source: "flightaware_aeroapi", TailNumber: "C-FIVM",
				Status: "scheduled", ScheduledDeparture: new(midnight.Add(5 * time.Minute)), ObservedAt: midnight.Add(-time.Minute),
				AircraftRegistry: &feeds.RegistryAircraftDetails{Source: feeds.CCARRegistrySource, Model: "777-333ER", Age: new(18)}}
			if err := store.Observe(ctx, prior, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			owner := friendInstallation(t, store)
			if err := store.Track(ctx, owner, prior.ID, ""); err != nil {
				t.Fatal(err)
			}
			next := prior
			next.Status, next.ObservedAt = "airborne", midnight.Add(time.Hour)
			next.ActualDeparture, next.AircraftRegistry = new(midnight.Add(tc.offset)), nil
			if err := store.Observe(ctx, next, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			got, err := store.DetailsForInstallation(ctx, owner, prior.ID)
			if err != nil || (got.Flight.AircraftRegistry != nil) != tc.retains {
				t.Fatalf("carried registry metadata = %+v, error = %v", got.Flight.AircraftRegistry, err)
			}
		})
	}
}
