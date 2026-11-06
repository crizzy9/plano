package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/api"
	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/demodata"
	"github.com/crizzy9/plano/internal/feeds"
)

type evolvingImportSource struct {
	demodata.Source
	secondLeg       bool
	firstLegLookups int
	lookups         []string
}

func (s *evolvingImportSource) Search(ctx context.Context, number string, date time.Time) ([]feeds.Flight, error) {
	s.lookups = append(s.lookups, number)
	if number == "UA123" {
		s.firstLegLookups++
	}
	if number == "UA456" && !s.secondLeg {
		return []feeds.Flight{}, nil
	}
	return s.Source.Search(ctx, number, date)
}

func TestEmailRetryOnlyResolvesUnfinishedLegs(t *testing.T) {
	ctx := t.Context()
	store, err := core.Open(filepath.Join(t.TempDir(), "plano.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	installation, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, installation)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := store.RotateEmailRelay(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	source := &evolvingImportSource{}
	handler := (api.Server{Store: store, Source: source, Demo: true, Logger: slog.New(slog.DiscardHandler)}).Handler()
	const email = "Subject: Itinerary\r\nContent-Type: text/plain\r\n\r\n" +
		"UA123 | Departure: 5 January 2027\r\nUA456 | Departure: 7 January 2027\r\n"
	forward := func(body string, want int) {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/webhooks/email", strings.NewReader(body))
		r.Header.Set("Content-Type", "message/rfc822")
		r.Header.Set("Authorization", "Bearer "+relay)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("email delivery = %d (%s), want %d", w.Code, w.Body, want)
		}
	}
	forward(email, http.StatusServiceUnavailable)
	if err := store.Untrack(ctx, owner, "demo-UA123-2027-01-05"); err != nil {
		t.Fatal(err)
	}
	forward(email, http.StatusServiceUnavailable)
	source.secondLeg = true
	forward(email, http.StatusNoContent)
	forward(email, http.StatusNoContent)
	const cancellation = "Subject: Calendar cancellation\r\nContent-Type: text/calendar; method=CANCEL\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:CANCEL\r\nBEGIN:VEVENT\r\nUID:fixture-cancel\r\n" +
		"DTSTART:20270105T150000Z\r\nSUMMARY:UA123\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	forward(cancellation, http.StatusNoContent)
	flights, err := store.Tracked(ctx, owner)
	if err != nil || len(flights) != 1 || flights[0].ID != "demo-UA456-2027-01-07" || source.firstLegLookups != 1 {
		t.Fatalf("retry tracked %+v after %d first-leg lookups, error = %v", flights, source.firstLegLookups, err)
	}
}

func TestEmailWebhookCancellationWithReplacement(t *testing.T) {
	ctx := t.Context()
	store, err := core.Open(filepath.Join(t.TempDir(), "plano.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	installation, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, installation)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := store.RotateEmailRelay(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	source := &evolvingImportSource{secondLeg: true}
	handler := (api.Server{Store: store, Source: source, Demo: true, Logger: slog.New(slog.DiscardHandler)}).Handler()
	forward := func(subject, body string) {
		t.Helper()
		email := "Subject: " + subject + "\r\nContent-Type: text/plain\r\n\r\n" + body
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/webhooks/email", strings.NewReader(email))
		r.Header.Set("Content-Type", "message/rfc822")
		r.Header.Set("Authorization", "Bearer "+relay)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("email delivery = %d (%s), want 204", w.Code, w.Body)
		}
	}
	assertTracked := func(want ...string) {
		t.Helper()
		flights, err := store.Tracked(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, flight := range flights {
			got = append(got, flight.ID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("tracked flight IDs = %v, want %v", got, want)
		}
	}
	const cancelled = "Flight UA123 on 5 January 2027 has been cancelled"
	forward(cancelled, "Please contact the airline for assistance.\r\n")
	assertTracked()
	if len(source.lookups) != 0 {
		t.Fatalf("cancellation triggered provider lookups: %v", source.lookups)
	}
	forward("Itinerary change", cancelled+".\r\nReplacement flight UA456 | Departure: 7 January 2027\r\n")
	assertTracked("demo-UA456-2027-01-07")
	forward("Itinerary change", "Flight UA456 on 7 January 2027 has been cancelled.\r\n")
	assertTracked("demo-UA456-2027-01-07")
	if !slices.Equal(source.lookups, []string{"UA456"}) {
		t.Fatalf("provider lookups = %v, want only the replacement flight", source.lookups)
	}
}

func TestRelayReplayRevocationAndPrivateBookingLookup(t *testing.T) {
	store, err := core.Open(filepath.Join(t.TempDir(), "plano.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := &evolvingImportSource{}
	h := api.Server{Store: store, Source: source, PublicURL: "https://plano.example.org", BookingKey: []byte(strings.Repeat("f", 32)), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Demo: true}.Handler()
	request := func(method, path, token, kind, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("Content-Type", kind)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: status %d want %d (%s)", method, path, w.Code, want, w.Body)
		}
		return w
	}
	token := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var fields struct{ Token string }
		if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil || fields.Token == "" {
			t.Fatalf("invalid response token: %v", err)
		}
		return fields.Token
	}
	const jsonType = "application/json"
	owner := token(request(http.MethodPost, "/v1/installations", "", jsonType, `{}`, 201))
	other := token(request(http.MethodPost, "/v1/installations", "", jsonType, `{}`, 201))
	relay := token(request(http.MethodPost, "/v1/import/email-relay", owner, jsonType, `{}`, 201))
	const body = "Subject: itinerary\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		`<p>Booking reference: ZXC12B</p><table>
<tr><th>Flight</th><th>Departure Date</th><th>Departure Time</th><th>Arrival</th></tr>
<tr><td>UA123</td><td>5 January 2027</td><td>7:00 AM</td><td>5 January 2027<br>3:00 PM</td></tr>
<tr><td>UA456</td><td>7 January 2027</td><td>5:00 PM</td><td>8 January 2027<br>5:00 AM</td></tr>
</table>`
	request(http.MethodPost, "/v1/webhooks/email", owner, "message/rfc822", body, 401)
	request(http.MethodGet, "/v1/tracked", relay, jsonType, "", 401)
	partial := request(http.MethodPost, "/v1/webhooks/email", relay, "message/rfc822", body, 503)
	if partial.Header().Get("Retry-After") != "300" {
		t.Fatal("partial import is not retryable")
	}
	tracked := request(http.MethodGet, "/v1/tracked", owner, jsonType, "", 200).Body.String()
	if !strings.Contains(tracked, "demo-UA123") || strings.Contains(tracked, "demo-UA456") {
		t.Fatal(tracked)
	}
	source.secondLeg = true
	request(http.MethodPost, "/v1/webhooks/email", relay, "message/rfc822", body, 204)
	request(http.MethodPost, "/v1/webhooks/email", relay, "message/rfc822", body, 204)
	own := request(http.MethodPost, "/v1/bookings/lookup", owner, jsonType, `{"locator":"zxc12b"}`, 200).Body.String()
	if !strings.Contains(own, "demo-UA123-2027-01-05") || !strings.Contains(own, "demo-UA456-2027-01-07") || strings.Contains(own, "ZXC12B") {
		t.Fatal(own)
	}
	foreign := request(http.MethodPost, "/v1/bookings/lookup", other, jsonType, `{"locator":"ZXC12B"}`, 200).Body.String()
	if !strings.Contains(foreign, `"flights":[]`) {
		t.Fatal(foreign)
	}
	request(http.MethodDelete, "/v1/import/email-relay", owner, jsonType, "", 204)
	request(http.MethodPost, "/v1/webhooks/email", relay, "message/rfc822", body, 401)
	request(http.MethodDelete, "/v1/tracked/demo-UA123-2027-01-05", owner, jsonType, "", 204)
	own = request(http.MethodPost, "/v1/bookings/lookup", owner, jsonType, `{"locator":"ZXC12B"}`, 200).Body.String()
	if strings.Contains(own, "demo-UA123") || !strings.Contains(own, "demo-UA456") {
		t.Fatal(own)
	}
}
