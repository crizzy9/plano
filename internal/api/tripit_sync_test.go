package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/demodata"
	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/itinerary"
)

func newImportTestServer(t *testing.T) Server {
	t.Helper()
	store, err := core.Open(filepath.Join(t.TempDir(), "plano.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return Server{Store: store, Source: demodata.Source{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

type tripItTestFlightSource struct {
	demodata.Source
	search func(context.Context, string, time.Time) ([]feeds.Flight, error)
}

func (s tripItTestFlightSource) Search(ctx context.Context, number string, date time.Time) ([]feeds.Flight, error) {
	return s.search(ctx, number, date)
}

func TestTripItSyncResumesAtFirstUnattemptedLegAfterBudgetExpires(t *testing.T) {
	service := newImportTestServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var searches []string
	service.Source = tripItTestFlightSource{search: func(_ context.Context, number string, _ time.Time) ([]feeds.Flight, error) {
		searches = append(searches, number)
		if number == "UA123" && len(searches) == 1 {
			cancel()
		}
		return nil, nil
	}}
	legs := []itinerary.Leg{{Number: "UA123", Date: "2026-11-07"}, {Number: "UA456", Date: "2026-11-07"}}
	result, more, err := service.resolveTripItBatch(ctx, legs, map[string]bool{}, 0)
	if !errors.Is(err, context.Canceled) || !more || result.Cursor != 1 || len(searches) != 1 {
		t.Fatalf("unexpected resume state %+v, more=%v, searches=%v, err=%v", result, more, searches, err)
	}
	if _, _, err := service.resolveTripItBatch(t.Context(), legs, map[string]bool{}, result.Cursor); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(searches, []string{"UA123", "UA456"}) {
		t.Fatalf("unexpected resumed order: %v", searches)
	}
}

func TestTripItSyncRotatesAfterFirstLookupTimesOut(t *testing.T) {
	service := newImportTestServer(t)
	_, owner := newImportTestInstallation(t, service.Store)
	ctx := t.Context()
	now := time.Now().UTC()
	const date = "2026-11-07"
	legs := []itinerary.Leg{{Number: "UA123", Date: date}, {Number: "UA456", Date: date}}
	if err := service.Store.SetTripItSubscription(ctx, owner, []byte("fixture"), now); err != nil {
		t.Fatal(err)
	}
	claim := func(at time.Time) core.TripItJob {
		t.Helper()
		job, err := service.Store.ClaimTripItSync(ctx, at)
		if err != nil || job == nil {
			t.Fatalf("claim failed: %v", err)
		}
		return *job
	}
	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	var searches []string
	service.Source = tripItTestFlightSource{search: func(ctx context.Context, number string, day time.Time) ([]feeds.Flight, error) {
		searches = append(searches, number)
		if number == "UA123" {
			if len(searches) == 1 {
				cancel()
				return nil, context.DeadlineExceeded
			}
			return nil, nil
		}
		return (demodata.Source{}).Search(ctx, number, day)
	}}
	first := claim(now)
	result, more, err := service.resolveTripItBatch(attempt, legs, map[string]bool{}, first.Cursor)
	if !errors.Is(err, context.DeadlineExceeded) || more || result.Cursor != 1 || !slices.Equal(searches, []string{"UA123"}) {
		t.Fatalf("first attempt: %+v, more=%v, searches=%v, err=%v", result, more, searches, err)
	}
	result.Unavailable = true
	next := now.Add(time.Hour)
	if err := service.Store.FinishTripItSync(ctx, first, result, now, next); err != nil {
		t.Fatal(err)
	}
	second := claim(next)
	result, more, err = service.resolveTripItBatch(ctx, legs, map[string]bool{}, second.Cursor)
	if err != nil || more || result.Cursor != 0 || !slices.Equal(searches, []string{"UA123", "UA456"}) {
		t.Fatalf("resumed attempt: %+v, more=%v, searches=%v, err=%v", result, more, searches, err)
	}
	retry := next.Add(time.Hour)
	if err := service.Store.FinishTripItSync(ctx, second, result, next, retry); err != nil {
		t.Fatal(err)
	}
	tracked, err := service.Store.Tracked(ctx, owner)
	if err != nil || len(tracked) != 1 || tracked[0].ID != "demo-UA456-"+date {
		t.Fatalf("tracked flights: %+v, err=%v", tracked, err)
	}
	seen, err := service.Store.TripItImportedLegs(ctx, owner, date)
	if err != nil {
		t.Fatal(err)
	}
	third := claim(retry)
	result, more, err = service.resolveTripItBatch(ctx, legs, seen, third.Cursor)
	if err != nil || more || !slices.Equal(searches, []string{"UA123", "UA456", "UA123"}) {
		t.Fatalf("next scheduled attempt: %+v, more=%v, searches=%v, err=%v", result, more, searches, err)
	}
}

func newImportTestInstallation(t *testing.T, store *core.Store) (token, owner string) {
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

func importTestRequest(t *testing.T, handler http.Handler, method, path, token, body string, status int) string {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s returned %d, want %d: %s", method, path, w.Code, status, w.Body)
	}
	return w.Body.String()
}

func TestTripItSubscriptionIsPrivateAndDoesNotRetrackRemovedFlights(t *testing.T) {
	service := newImportTestServer(t)
	var err error
	service.TripItSync, err = NewTripItSync(bytes.Repeat([]byte("k"), 32), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().AddDate(0, 0, 1)
	flightID := "demo-UA123-" + day.Format(time.DateOnly)
	const path = "/v1/import/tripit/subscription"
	const privatePath = "/feed/ical/private/sensitive-fixture/tripit.ics"
	const input = `{"url":"webcal://tripit.com` + privatePath + `"}`
	calendar := fmt.Sprintf("BEGIN:VCALENDAR\r\n"+
		"BEGIN:VEVENT\r\nUID:trip@example.test\r\nSEQUENCE:1\r\nDTSTART:%sT150000Z\r\nSUMMARY:Flight UA123\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:trip@example.test\r\nSEQUENCE:2\r\nDTSTAMP:20200101T120000Z\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:trip@example.test\r\nSEQUENCE:2\r\nDTSTAMP:20200101T130000Z\r\nDTSTART:%sT150000Z\r\nSUMMARY:Flight UA123\r\nEND:VEVENT\r\n"+
		"END:VCALENDAR\r\n", day.AddDate(0, 0, 1).Format("20060102"), day.Format("20060102"))
	requests := 0
	service.TripItSync.client = &http.Client{Transport: tripItTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Scheme != "https" || r.URL.Host != "www.tripit.com" || r.URL.Path != privatePath {
			t.Fatal("unexpected calendar fetch target")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(calendar))}, nil
	})}
	handler := service.Handler()
	token, owner := newImportTestInstallation(t, service.Store)
	otherToken, otherOwner := newImportTestInstallation(t, service.Store)
	importTestRequest(t, handler, "PUT", path, "", input, 401)
	importTestRequest(t, handler, "PUT", path, token, `{"url":"https://example.org/tripit.ics"}`, 400)
	importTestRequest(t, handler, "PUT", path, token, input, 204)
	status := importTestRequest(t, handler, "GET", path, token, "", 200)
	if !strings.Contains(status, `"status":"pending"`) || strings.Contains(status, "sensitive-fixture") {
		t.Fatalf("unexpected pending status: %s", status)
	}
	other := importTestRequest(t, handler, "GET", path, otherToken, "", 200)
	if !strings.Contains(other, `"enabled":false`) {
		t.Fatalf("subscription leaked across installations: %s", other)
	}
	claim := func(at time.Time) core.TripItJob {
		t.Helper()
		job, err := service.Store.ClaimTripItSync(t.Context(), at)
		if err != nil || job == nil {
			t.Fatalf("claim failed: %v", err)
		}
		return *job
	}
	job := claim(time.Now())
	if job.Owner != owner || bytes.Contains(job.EncryptedURL, []byte("sensitive-fixture")) {
		t.Fatal("private URL was not stored as installation-scoped ciphertext")
	}
	if _, err := service.TripItSync.cipher.Open(nil, nil, job.EncryptedURL, []byte(otherOwner)); err == nil {
		t.Fatal("another owner could decrypt the feed URL")
	}
	service.syncTripItJob(t.Context(), job)
	status = importTestRequest(t, handler, "GET", path, token, "", 200)
	if !strings.Contains(status, `"status":"synced"`) || !strings.Contains(status, `"matched":1`) {
		t.Fatalf("unexpected completed status: %s", status)
	}
	oldFlightID := "demo-UA123-" + day.AddDate(0, 0, 1).Format(time.DateOnly)
	if tracked := importTestRequest(t, handler, "GET", "/v1/tracked", token, "", 200); !strings.Contains(tracked, flightID) || strings.Contains(tracked, oldFlightID) {
		t.Fatalf("tracking does not match the current revision: %s", tracked)
	}
	importTestRequest(t, handler, "DELETE", "/v1/tracked/"+flightID, token, "", 204)
	service.syncTripItJob(t.Context(), claim(time.Now().Add(time.Hour)))
	if tracked := importTestRequest(t, handler, "GET", "/v1/tracked", token, "", 200); strings.Contains(tracked, flightID) || requests != 2 {
		t.Fatalf("unexpected repeat import: %s, %d fetches", tracked, requests)
	}
	stale := claim(time.Now().Add(time.Hour))
	importTestRequest(t, handler, "PUT", path, token, input, 204)
	finishStale := func(job core.TripItJob) {
		t.Helper()
		now := time.Now()
		result := core.TripItSyncResult{Matches: []core.TripItMatch{{Key: "stale", Date: day.Format(time.DateOnly), FlightID: flightID}}}
		if err := service.Store.FinishTripItSync(t.Context(), job, result, now, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if tracked := importTestRequest(t, handler, "GET", "/v1/tracked", token, "", 200); strings.Contains(tracked, flightID) {
			t.Fatalf("stale job modified tracking: %s", tracked)
		}
	}
	finishStale(stale)
	stale = claim(time.Now())
	service.TripItSync = nil
	paused := service.Handler()
	status = importTestRequest(t, paused, "GET", path, token, "", 200)
	if !strings.Contains(status, `"available":false`) || !strings.Contains(status, `"enabled":true`) {
		t.Fatalf("unexpected paused status: %s", status)
	}
	importTestRequest(t, paused, "DELETE", path, token, "", 204)
	finishStale(stale)
	if job, err := service.Store.ClaimTripItSync(t.Context(), time.Now().Add(24*time.Hour)); job != nil || err != nil {
		t.Fatalf("removed subscription was claimable: %v", err)
	}
}

func TestTripItSubscriptionRemembersFlightsAcrossCorrectionsAndRestart(t *testing.T) {
	service := newImportTestServer(t)
	var err error
	service.TripItSync, err = NewTripItSync(bytes.Repeat([]byte("k"), 32), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	departure := day.Add(15 * time.Hour)
	const originalID, rebookedID = "tripit-corrected-flight", "tripit-rebooked-flight"
	flightID := originalID
	var sequence, searches int
	service.TripItSync.client = &http.Client{Transport: tripItTransport(func(*http.Request) (*http.Response, error) {
		calendar := fmt.Sprintf("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:trip@example.test\r\nSEQUENCE:%d\r\nDTSTART:%s\r\nSUMMARY:Flight UA123\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
			sequence, departure.Format("20060102T150405Z"))
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(calendar))}, nil
	})}
	service.Source = tripItTestFlightSource{search: func(_ context.Context, number string, date time.Time) ([]feeds.Flight, error) {
		searches++
		if number != "UA123" || !date.Equal(day) {
			t.Fatalf("unexpected flight search: %s %s", number, date.Format(time.DateOnly))
		}
		return []feeds.Flight{{
			ID: flightID, Number: number, Origin: "SFO", Destination: "EWR", Status: "scheduled",
			ScheduledDeparture: &departure, ObservedAt: time.Now().UTC(),
		}}, nil
	}}
	token, owner := newImportTestInstallation(t, service.Store)
	const path = "/v1/import/tripit/subscription"
	const input = `{"url":"webcal://tripit.com/feed/ical/fixture/tripit.ics"}`
	handler := service.Handler()
	set := func() { importTestRequest(t, handler, "PUT", path, token, input, 204) }
	sync := func(wantSearches int, wantTracked ...string) {
		t.Helper()
		job, err := service.Store.ClaimTripItSync(ctx, time.Now().Add(time.Hour))
		if err != nil || job == nil {
			t.Fatalf("claim failed: %v", err)
		}
		service.syncTripItJob(ctx, *job)
		status, err := service.Store.TripItStatus(ctx, owner)
		if err != nil || status.Status != "synced" || status.Matched != 1 || searches != wantSearches {
			t.Fatalf("unexpected sync status %+v, searches=%d (want %d), err=%v", status, searches, wantSearches, err)
		}
		flights, err := service.Store.Tracked(ctx, owner)
		if err != nil || len(flights) != len(wantTracked) {
			t.Fatalf("tracked flights: %+v, want %v, err=%v", flights, wantTracked, err)
		}
		for i, want := range wantTracked {
			if flights[i].ID != want {
				t.Fatalf("tracked flight %q, want %q", flights[i].ID, want)
			}
		}
	}

	set()
	sync(1, originalID)
	importTestRequest(t, handler, "DELETE", "/v1/tracked/"+originalID, token, "", 204)
	sequence++
	departure = departure.Add(30 * time.Minute)
	sync(2)

	importTestRequest(t, handler, "DELETE", path, token, "", 204)
	set()
	sync(2)

	sequence++
	departure = departure.Add(30 * time.Minute)
	flightID = rebookedID
	sync(3, rebookedID)
}

func TestTripItSyncContinuesPastAnUnmatchedBatch(t *testing.T) {
	for _, matchLast := range []bool{true, false} {
		name := "later match"
		if !matchLast {
			name = "all unmatched"
		}
		t.Run(name, func(t *testing.T) {
			service := newImportTestServer(t)
			const interval = 24 * time.Hour
			var err error
			service.TripItSync, err = NewTripItSync(bytes.Repeat([]byte("k"), 32), interval)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			day := time.Now().UTC().AddDate(0, 0, 1)
			var calendar strings.Builder
			calendar.WriteString("BEGIN:VCALENDAR\r\n")
			addLeg := func(number string) {
				fmt.Fprintf(&calendar, "BEGIN:VEVENT\r\nUID:%s\r\nDTSTART;VALUE=DATE:%s\r\nSUMMARY:Flight %s\r\nEND:VEVENT\r\n",
					number, day.Format("20060102"), number)
			}
			for i := range itinerary.MaxFlights {
				addLeg(fmt.Sprintf("UA%d", 101+i))
			}
			addLeg("UA123")
			calendar.WriteString("END:VCALENDAR\r\n")
			service.TripItSync.client = &http.Client{Transport: tripItTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(calendar.String()))}, nil
			})}
			var searches []string
			service.Source = tripItTestFlightSource{search: func(ctx context.Context, number string, date time.Time) ([]feeds.Flight, error) {
				searches = append(searches, number)
				if matchLast && number == "UA123" {
					return (demodata.Source{}).Search(ctx, number, date)
				}
				return nil, nil
			}}
			token, owner := newImportTestInstallation(t, service.Store)
			importTestRequest(t, service.Handler(), "PUT", "/v1/import/tripit/subscription", token,
				`{"url":"webcal://tripit.com/feed/ical/fixture/tripit.ics"}`, 204)
			claim := func(at time.Time) core.TripItJob {
				t.Helper()
				job, err := service.Store.ClaimTripItSync(ctx, at)
				if err != nil || job == nil {
					t.Fatalf("claim failed: %v", err)
				}
				return *job
			}
			status := func() core.TripItStatus {
				t.Helper()
				status, err := service.Store.TripItStatus(ctx, owner)
				if err != nil || status.LastSyncAt == nil || status.NextSyncAt == nil || status.Status != "synced" {
					t.Fatalf("unexpected sync status %+v, err=%v", status, err)
				}
				return status
			}
			service.syncTripItJob(ctx, claim(time.Now()))
			first := status()
			if delay := first.NextSyncAt.Sub(*first.LastSyncAt); delay != 2*time.Minute || first.Matched != 0 ||
				first.Unmatched != itinerary.MaxFlights+1 || len(searches) != itinerary.MaxFlights {
				t.Fatalf("first batch status %+v, delay=%v, searches=%v", first, delay, searches)
			}
			service.syncTripItJob(ctx, claim(*first.NextSyncAt))
			second := status()
			wantMatched := 0
			if matchLast {
				wantMatched = 1
			}
			if delay := second.NextSyncAt.Sub(*second.LastSyncAt); delay != interval || second.Matched != wantMatched ||
				second.Unmatched != itinerary.MaxFlights+1-wantMatched || !slices.Equal(searches[itinerary.MaxFlights:], []string{"UA123"}) {
				t.Fatalf("completed sweep status %+v, delay=%v, searches=%v", second, delay, searches)
			}
			tracked, err := service.Store.Tracked(ctx, owner)
			if err != nil || len(tracked) != wantMatched || matchLast && tracked[0].ID != "demo-UA123-"+day.Format(time.DateOnly) {
				t.Fatalf("tracked flights: %+v, err=%v", tracked, err)
			}
			if next := claim(*second.NextSyncAt); next.Cursor != 0 {
				t.Fatalf("next scheduled sweep cursor = %d, want 0", next.Cursor)
			}
		})
	}
}

type reservationTestSource func(context.Context, feeds.ReservationQuery) ([]feeds.ReservationLeg, error)

func (f reservationTestSource) Lookup(ctx context.Context, query feeds.ReservationQuery) ([]feeds.ReservationLeg, error) {
	return f(ctx, query)
}

func TestPartnerBookingOnlyReturnsPublicFlightsForReview(t *testing.T) {
	service := newImportTestServer(t)
	token, _ := newImportTestInstallation(t, service.Store)
	const path = "/v1/bookings/resolve"
	const input = `{"carrier":"ua","locator":"ab12cd","lastName":" Example "}`
	importTestRequest(t, service.Handler(), "POST", path, token, input, 503)
	service.ReservationSource = reservationTestSource(func(_ context.Context, query feeds.ReservationQuery) ([]feeds.ReservationLeg, error) {
		if query.Carrier != "UA" || query.Locator != "AB12CD" || query.Surname != "Example" {
			t.Fatal("gateway received an unexpected query")
		}
		return []feeds.ReservationLeg{{Number: "UA123", Date: "2026-11-07"}}, nil
	})
	handler := service.Handler()
	importTestRequest(t, handler, "POST", path, "", input, 401)
	importTestRequest(t, handler, "POST", path, token, `{"carrier":"ua","locator":"abc","lastName":"Example"}`, 400)
	const id = "demo-UA123-2026-11-07"
	result := importTestRequest(t, handler, "POST", path, token, input, 200)
	if !strings.Contains(result, id) || strings.Contains(result, "AB12CD") || strings.Contains(result, "Example") {
		t.Fatalf("unexpected response: %s", result)
	}
	if tracked := importTestRequest(t, handler, "GET", "/v1/tracked", token, "", 200); strings.Contains(tracked, id) {
		t.Fatalf("partner result tracked without review: %s", tracked)
	}
}

func TestPartnerBookingResolvesRepeatedDesignatorByRoute(t *testing.T) {
	service := newImportTestServer(t)
	token, owner := newImportTestInstallation(t, service.Store)
	const date = "2026-11-07"
	const path = "/v1/bookings/resolve"
	const input = `{"carrier":"UA","locator":"AB12CD","lastName":"Example"}`
	const body = `{"version":1,"authorized":true,"legs":[
		{"number":"UA123","date":"2026-11-07","origin":"KSFO","destination":"DEN","departure":"2026-11-07T16:00:00-08:00"},
		{"number":"UA123","date":"2026-11-07","origin":"DEN","destination":"KORD"},
		{"number":"UA123","date":"2026-11-07","origin":"SFO","destination":"JFK"}
	]}`
	gateway, err := feeds.NewReservationGateway("https://reservation.invalid/lookup", "fixture-token", &http.Client{
		Transport: tripItTransport(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	service.ReservationSource = gateway
	departure := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	shifted := departure.Add(10 * time.Minute)
	onward := departure.Add(4 * time.Hour)
	later := departure.Add(2 * time.Hour)
	first := feeds.Flight{ID: "pnr-first", Number: "UA123", Origin: "SFO", OriginICAO: "KSFO", Destination: "DEN",
		Status: "scheduled", ScheduledDeparture: &shifted, ObservedAt: departure}
	second := feeds.Flight{ID: "pnr-second", Number: "UA123", Origin: "DEN", Destination: "KORD",
		Status: "scheduled", ScheduledDeparture: &onward, ObservedAt: departure}
	wrongTime := first
	wrongTime.ID, wrongTime.ScheduledDeparture = "pnr-wrong-time", &later
	wrongRoute := first
	wrongRoute.ID, wrongRoute.Destination, wrongRoute.ScheduledDeparture = "pnr-wrong-route", "DFW", &departure
	service.Source = tripItTestFlightSource{search: func(_ context.Context, number string, day time.Time) ([]feeds.Flight, error) {
		if number != "UA123" {
			t.Fatalf("unexpected flight number %q", number)
		}
		switch day.Format(time.DateOnly) {
		case date:
			return []feeds.Flight{wrongRoute, first, second, wrongTime}, nil
		case "2026-11-08":
			return []feeds.Flight{first}, nil
		default:
			return nil, nil
		}
	}}
	response := importTestRequest(t, service.Handler(), "POST", path, token, input, 200)
	var result importResult
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Flights) != 2 || result.Flights[0].ID != first.ID || result.Flights[1].ID != second.ID ||
		!slices.Equal(result.Unmatched, []string{"UA123 · " + date}) {
		t.Fatalf("unexpected reservation resolution: %s", response)
	}
	if tracked, err := service.Store.Tracked(t.Context(), owner); err != nil || len(tracked) != 0 {
		t.Fatalf("reservation lookup modified tracking: %+v, err=%v", tracked, err)
	}
}
