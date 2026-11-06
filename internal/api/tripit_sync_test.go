package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	if len(searches) != 3 || searches[1] != "UA456" {
		t.Fatalf("unexpected resumed order: %v", searches)
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
		"BEGIN:VEVENT\r\nUID:trip@example.test\r\nSEQUENCE:2\r\nDTSTART:%sT150000Z\r\nSUMMARY:Flight UA123\r\nEND:VEVENT\r\n"+
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
