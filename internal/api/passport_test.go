package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	otherToken, other := passportInstallation(t, store)
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
	handler := (Server{Store: store, Source: provider, Demo: true, Logger: slog.New(slog.DiscardHandler)}).Handler()
	first := passportRequest(t, handler, token, url.Values{"limit": {"50"}}, http.StatusOK)
	var summary passportPagedResponse
	if err := json.Unmarshal(first, &summary); err != nil || len(summary.Flights) != 50 || len(summary.Segments) != 1 ||
		summary.Stats != passport.Stats || summary.Coverage != passport.Coverage || summary.NextCursor == "" {
		t.Fatalf("bounded Passport flights=%d segments=%d stats=%+v coverage=%+v error=%v", len(summary.Flights), len(summary.Segments), summary.Stats, summary.Coverage, err)
	}
	// Continuations must skip provider requests even on a live-mode server.
	handler = (Server{Store: store, Source: provider, Logger: slog.New(slog.DiscardHandler)}).Handler()
	query := url.Values{"limit": {"100"}, "cursor": {summary.NextCursor}}
	var isolated passportHistoryPage
	if err := json.Unmarshal(passportRequest(t, handler, otherToken, query, http.StatusOK), &isolated); err != nil ||
		len(isolated.Flights) != 1 || isolated.Flights[0].ID != "other-arrived" || isolated.NextCursor != "" {
		t.Fatalf("another installation's cursor leaked history: %+v, %v", isolated, err)
	}
	seen := append([]passportHistoryFlight{}, summary.Flights...)
	for range 12 {
		var page passportHistoryPage
		body := passportRequest(t, handler, token, query, http.StatusOK)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil || fields["stats"] != nil || fields["segments"] != nil || fields["coverage"] != nil {
			t.Fatalf("older history recomputed the summary: %s, %v", body, err)
		}
		if err := json.Unmarshal(body, &page); err != nil || len(page.Flights) > 100 || len(page.Flights) == 0 {
			t.Fatalf("invalid older page with %d flights, error=%v", len(page.Flights), err)
		}
		seen = append(seen, page.Flights...)
		if page.NextCursor == "" {
			break
		}
		query.Set("cursor", page.NextCursor)
	}
	if len(seen) != flights {
		t.Fatalf("paged %d of %d completed flights", len(seen), flights)
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("older history consulted the airport provider %d times", got)
	}
	for i, flight := range seen {
		if flight.ID != fmt.Sprintf("complete-%04d", i) || flight.HistoryKey != "l."+flight.ID {
			t.Fatalf("flight %d = %s", i, flight.ID)
		}
	}
}

func passportRequest(t *testing.T, handler http.Handler, token string, query url.Values, want int) []byte {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/passport?"+query.Encode(), nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("Passport query %q status %d, want %d: %s", query.Encode(), response.Code, want, response.Body)
	}
	return response.Body.Bytes()
}

func TestPassportPagedCoverageMatchesFullHistory(t *testing.T) {
	store := openPassportStore(t, filepath.Join(t.TempDir(), "plano.db"))
	token, owner := passportInstallation(t, store)
	now := time.Now().UTC()
	for _, flight := range []feeds.Flight{
		{ID: "SFO-EWR", Origin: "SFO", Destination: "EWR", DistanceMiles: new(100), TailNumber: "N-123AA"},
		{ID: "EWR-SFO", Origin: "EWR", Destination: "SFO", TailNumber: "n123aa"},
		{ID: "LHR-missing", Origin: "LHR", Destination: "AAA"},
	} {
		flight.Status, flight.ObservedAt = "arrived", now
		addPassportFlight(t, store, owner, flight)
	}
	legacy := readPassport(t, store, nil, true, token)
	handler := (Server{Store: store, Demo: true, Logger: slog.New(slog.DiscardHandler)}).Handler()
	var paged passportPagedResponse
	if err := json.Unmarshal(passportRequest(t, handler, token, url.Values{"limit": {"1"}}, http.StatusOK), &paged); err != nil ||
		paged.Stats != legacy.Stats || paged.Stats.Aircraft != 1 || paged.Coverage != legacy.Coverage ||
		paged.Coverage.MissingCoordinatesFlights != 1 || paged.Coverage.MissingDistanceFlights != 1 ||
		len(paged.Flights) != 1 || len(paged.Segments) != 1 || paged.NextCursor == "" {
		t.Fatalf("paged coverage = %+v, legacy = %+v, error = %v", paged, legacy, err)
	}
	missingArrival := base64.RawURLEncoding.EncodeToString([]byte(`{"ID":"l.LHR-missing"}`))
	for _, raw := range []string{"cursor=bogus", "limit=0", "limit=101", "limit=1&cursor=bogus", "limit=1&cursor=", "limit=1&cursor=" + missingArrival} {
		query, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		passportRequest(t, handler, token, query, http.StatusBadRequest)
	}
}

func TestPassportManualMeasurementsAndCoordinatePrivacy(t *testing.T) {
	store := openPassportStore(t, filepath.Join(t.TempDir(), "plano.db"))
	token, owner := passportInstallation(t, store)
	now := time.Now().UTC()
	for i, flight := range []feeds.Flight{
		{Origin: "Q0X", Destination: "SFO", HistoryDate: "2020-03-02", TailNumber: "N-123AB", HistoryDurationMinutes: new(90), HistoryArrivalDelayMinutes: new(0)},
		{Origin: "SFO", Destination: "Q0Y", HistoryDate: "2020-02-01"},
		{Origin: "SFO", Destination: "Q0Z", HistoryDate: "2020-02-02", HistoryDurationMinutes: new(150), HistoryArrivalDelayMinutes: new(23)},
	} {
		flight.ID = fmt.Sprintf("archive-%064x", i+1)
		flight.Status, flight.Source, flight.ObservedAt = "arrived", feeds.PassportManualSource, now
		if created, err := store.CreateManualPassportFlight(t.Context(), owner, flight); err != nil || !created {
			t.Fatalf("manual flight %d created=%t, error=%v", i, created, err)
		}
	}
	provider := &passportSourceFixture{lookup: func(code string) (feeds.Airport, error) {
		switch code {
		case "Q0X":
			return feeds.Airport{Code: code, Latitude: new(36.0), Longitude: new(-118.0)}, nil
		case "Q0Y":
			return feeds.Airport{Code: code, Latitude: new(41.0), Longitude: new(-74.0)}, nil
		default:
			t.Errorf("unexpected provider lookup for %s", code)
			return feeds.Airport{}, feeds.ErrNotFound
		}
	}}
	manual := readPassport(t, store, provider, false, token)
	if provider.calls.Load() != 0 || manual.Stats != (passportStats{Flights: 3, Hours: 4, Aircraft: 1, DelayMinutes: 23}) ||
		manual.Coverage != (passportCoverage{MissingCoordinatesFlights: 3, MissingDistanceFlights: 3, MissingDurationFlights: 1, MissingDelayFlights: 1}) {
		t.Fatalf("manual Passport stats=%+v coverage=%+v lookups=%d", manual.Stats, manual.Coverage, provider.calls.Load())
	}

	// Exercise both possible orders of the mixed route representatives.
	for i, flight := range []feeds.Flight{
		{Origin: "Q0X", Destination: "SFO", ActualArrival: new(time.Date(2020, 2, 2, 12, 0, 0, 0, time.UTC))},
		{Origin: "SFO", Destination: "Q0Y", ActualArrival: new(time.Date(2020, 4, 2, 12, 0, 0, 0, time.UTC))},
	} {
		flight.ID, flight.Status, flight.ObservedAt = fmt.Sprintf("tracked-%d", i), "arrived", now
		flight.ActualDeparture = new(flight.ActualArrival.Add(-time.Hour))
		flight.ScheduledArrival = new(flight.ActualArrival.Add(-15 * time.Minute))
		addPassportFlight(t, store, owner, flight)
	}
	handler := (Server{Store: store, Source: provider, Logger: slog.New(slog.DiscardHandler)}).Handler()
	var paged passportPagedResponse
	if err := json.Unmarshal(passportRequest(t, handler, token, url.Values{"limit": {"1"}}, http.StatusOK), &paged); err != nil {
		t.Fatal(err)
	}
	full := readPassport(t, store, provider, false, token)
	if provider.calls.Load() != 2 || paged.Stats != full.Stats || paged.Coverage != full.Coverage ||
		paged.Stats.Flights != 5 || paged.Stats.Hours != 6 || paged.Stats.DelayMinutes != 53 || paged.Stats.Miles <= 0 ||
		paged.Coverage != (passportCoverage{MissingCoordinatesFlights: 1, MissingDistanceFlights: 1, MissingDurationFlights: 1, MissingDelayFlights: 1}) ||
		len(paged.Segments) != 2 {
		t.Fatalf("mixed Passport stats=%+v coverage=%+v segments=%d lookups=%d", paged.Stats, paged.Coverage, len(paged.Segments), provider.calls.Load())
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
