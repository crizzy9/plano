package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/demodata"
	"github.com/crizzy9/plano/internal/feeds"
)

func openPassportStore(t *testing.T, path string) *core.Store {
	t.Helper()
	store, err := core.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func passportInstallation(t *testing.T, store *core.Store) (token, owner string) {
	t.Helper()
	token, err := store.CreateInstallation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err = store.Installation(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	return token, owner
}

func addPassportFlight(t *testing.T, store *core.Store, owner string, flight feeds.Flight) {
	t.Helper()
	if err := store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(t.Context(), owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
}

func readPassport(t *testing.T, store *core.Store, source feeds.FlightSource, demo bool, token string) passportResponse {
	t.Helper()
	handler := (Server{
		Store: store, Source: source, Demo: demo,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/v1/passport", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("Passport status = %d, body = %s", response.Code, response.Body)
	}
	var result passportResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

type passportSourceFixture struct {
	demodata.Source
	lookup func(code string) (feeds.Airport, error)
	calls  atomic.Int64
}

func (s *passportSourceFixture) Airport(_ context.Context, code string) (feeds.Airport, error) {
	s.calls.Add(1)
	return s.lookup(code)
}

func TestPassportLifetimeHistoryExceedsTrackedLimit(t *testing.T) {
	store := openPassportStore(t, filepath.Join(t.TempDir(), "plano.db"))
	token, owner := passportInstallation(t, store)
	_, other := passportInstallation(t, store)
	const flights = 1005
	now := time.Now().UTC()
	for i := range flights {
		arrive := now.Add(-time.Duration(i+1) * time.Hour)
		depart, scheduled := arrive.Add(-time.Hour), arrive.Add(-2*time.Minute)
		tail := "n123ab"
		if i%2 == 0 {
			tail = "N123AB"
		}
		addPassportFlight(t, store, owner, feeds.Flight{
			ID: fmt.Sprintf("complete-%04d", i), Origin: "SFO", Destination: "EWR",
			Status: "arrived", ObservedAt: now, TailNumber: tail, DistanceMiles: new(100),
			ActualDeparture: &depart, ActualArrival: &arrive, ScheduledArrival: &scheduled,
		})
	}
	addPassportFlight(t, store, owner, feeds.Flight{ID: "cancelled", Status: "cancelled", ObservedAt: now})
	addPassportFlight(t, store, other, feeds.Flight{ID: "other-arrived", Status: "arrived", ObservedAt: now})
	provider := &passportSourceFixture{lookup: func(string) (feeds.Airport, error) {
		return feeds.Airport{}, feeds.ErrUnavailable
	}}
	passport := readPassport(t, store, provider, true, token)
	if got := passport.Stats; got != (passportStats{flights, flights * 100, flights, 1, flights * 2}) {
		t.Fatalf("lifetime Passport totals = %+v", got)
	}
	if len(passport.Flights) != flights || len(passport.Segments) != flights || passport.Coverage != (passportCoverage{}) {
		t.Fatalf("lifetime Passport flights=%d segments=%d coverage=%+v", len(passport.Flights), len(passport.Segments), passport.Coverage)
	}
	if passport.Flights[0].ID != "complete-0000" || passport.Flights[flights-1].ID != "complete-1004" {
		t.Fatal("completed history was not ordered by arrival")
	}
	if provider.calls.Load() != 0 {
		t.Fatal("demo consulted a provider")
	}
}

func TestPassportCoordinatesFillOverRequestsAndSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plano.db")
	store := openPassportStore(t, path)
	token, owner := passportInstallation(t, store)
	const routes = 15
	now := time.Now().UTC()
	for i := range routes {
		addPassportFlight(t, store, owner, feeds.Flight{
			ID: fmt.Sprintf("route-%02d", i), Origin: fmt.Sprintf("X%02d", i), Destination: "A00",
			Status: "arrived", ObservedAt: now, DistanceMiles: new(100),
		})
	}
	provider := &passportSourceFixture{lookup: func(code string) (feeds.Airport, error) {
		return feeds.Airport{Code: code, Latitude: new(0.0), Longitude: new(0.0)}, nil
	}}

	first := readPassport(t, store, provider, false, token)
	if got := provider.calls.Load(); got != passportLookupBatch {
		t.Fatalf("provider called %d times during first request", got)
	}
	if first.Stats.Miles != routes*100 || first.Coverage != (passportCoverage{
		MissingCoordinatesFlights: 4, MissingDurationFlights: routes, MissingDelayFlights: routes, PendingAirportLookups: 4,
	}) || len(first.Segments) != 11 {
		t.Fatalf("first Passport stats=%+v coverage=%+v segments=%d", first.Stats, first.Coverage, len(first.Segments))
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openPassportStore(t, path)
	second := readPassport(t, store, provider, false, token)
	if got := provider.calls.Load(); got != routes+1 {
		t.Fatalf("expected restart to fetch only remaining airports, total calls = %d", got)
	}
	if len(second.Segments) != routes || second.Coverage != (passportCoverage{MissingDurationFlights: routes, MissingDelayFlights: routes}) {
		t.Fatalf("second Passport coverage=%+v segments=%d", second.Coverage, len(second.Segments))
	}
	if second.Segments[0].OriginLatitude != 0 || second.Segments[0].DestinationLongitude != 0 {
		t.Fatal("zero-degree coordinates were not preserved")
	}

	third := readPassport(t, store, provider, false, token)
	if got := provider.calls.Load(); got != routes+1 || len(third.Segments) != routes {
		t.Fatalf("complete cache got %d calls and %d segments", got, len(third.Segments))
	}
}

func TestPassportAirportFallbackAndNegativeBackoff(t *testing.T) {
	store := openPassportStore(t, filepath.Join(t.TempDir(), "plano.db"))
	token, owner := passportInstallation(t, store)
	now := time.Now().UTC()
	addPassportFlight(t, store, owner, feeds.Flight{
		ID: "distance-inferred", Origin: "SFO", OriginICAO: "KSFO", Destination: "EWR",
		Status: "arrived", ObservedAt: now,
	})
	addPassportFlight(t, store, owner, feeds.Flight{
		ID: "coordinates-unavailable", Origin: "SFO", Destination: "X99",
		Status: "arrived", ObservedAt: now, DistanceMiles: new(500),
	})
	addPassportFlight(t, store, owner, feeds.Flight{
		ID: "distance-unavailable", Origin: "SFO", Destination: "X99",
		Status: "arrived", ObservedAt: now,
	})
	provider := &passportSourceFixture{lookup: func(code string) (feeds.Airport, error) {
		switch code {
		case "SFO":
			return feeds.Airport{Code: code, Latitude: new(37.62), Longitude: new(-122.38)}, nil
		case "EWR":
			return feeds.Airport{Code: code, Latitude: new(40.69), Longitude: new(-74.17)}, nil
		default:
			return feeds.Airport{}, feeds.ErrNotFound
		}
	}}
	passport := readPassport(t, store, provider, false, token)
	if len(passport.Segments) != 1 || passport.Segments[0].DistanceMiles < 2500 || passport.Stats.Miles != passport.Segments[0].DistanceMiles+500 ||
		passport.Coverage.MissingDistanceFlights != 1 || passport.Coverage.MissingCoordinatesFlights != 2 || passport.Coverage.PendingAirportLookups != 0 {
		t.Fatalf("fallback Passport stats=%+v coverage=%+v segments=%+v", passport.Stats, passport.Coverage, passport.Segments)
	}
	calls := provider.calls.Load()
	readPassport(t, store, provider, false, token)
	if got := provider.calls.Load(); got != calls {
		t.Fatalf("cached negative result retried too soon, calls went from %d to %d", calls, got)
	}
}
