package api_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/api"
	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/demodata"
	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

type callbackSource struct {
	demodata.Source
	flight feeds.Flight
}

func (s callbackSource) Get(_ context.Context, id string) (feeds.Flight, error) {
	if s.flight.ID != id {
		return feeds.Flight{}, feeds.ErrNotFound
	}
	return s.flight, nil
}

type recordPush func(context.Context, push.Target, push.Message) error

func (send recordPush) Send(ctx context.Context, target push.Target, message push.Message) error {
	return send(ctx, target, message)
}

func trackTestFlight(t *testing.T, store *core.Store, flight feeds.Flight) string {
	t.Helper()
	ctx := context.Background()
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	return owner
}

func TestAeroCallbackDrivesOutboxFromAuthoritativeFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store, err := core.Open(filepath.Join(t.TempDir(), "callback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	departure := now.Add(30 * time.Hour)
	prior := feeds.Flight{ID: "UAL88-123-a", Source: "flightaware_aeroapi", Number: "UA88", Origin: "SFO", Destination: "EWR", Status: "scheduled", DepartureGate: "A2", ObservedAt: now.Add(-time.Minute), ScheduledDeparture: &departure}
	owner := trackTestFlight(t, store, prior)
	if err := store.Register(ctx, owner, core.Device{Platform: "ios", Kind: "alert", Token: "test-device"}, now); err != nil {
		t.Fatal(err)
	}
	updated := prior
	updated.DepartureGate, updated.ObservedAt = "B3", now
	source := callbackSource{flight: updated}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := strings.Repeat("a", 32)
	handler := api.Server{Store: store, Source: source, AeroCallbackKey: []byte(key), Logger: logger}.Handler()
	call := func(secret string) int {
		r := httptest.NewRequest(http.MethodPost, "/v1/webhooks/aeroapi", strings.NewReader(`{"alert_id":2,"event_code":"departure","flight":{"fa_flight_id":"UAL88-123-a","gate_origin":"CALLBACK-UNVERIFIED"}}`))
		r.Header.Set("Content-Type", "application/json")
		r.SetBasicAuth("plano", secret)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if code := call("bad"); code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status %d", code)
	}
	if code := call(key); code != http.StatusOK {
		t.Fatalf("callback status %d", code)
	}
	if code := call(key); code != http.StatusOK {
		t.Fatalf("duplicate status %d", code)
	}
	ended := make(chan struct{}, 2)
	pushed := make(chan push.Message, 2)
	go func() { defer func() { ended <- struct{}{} }(); store.RunAeroRefresh(ctx, source, logger) }()
	go func() {
		defer func() { ended <- struct{}{} }()
		store.RunDispatcher(ctx, recordPush(func(_ context.Context, _ push.Target, m push.Message) error { pushed <- m; return nil }), logger)
	}()
	defer func() { cancel(); <-ended; <-ended }()
	select {
	case m := <-pushed:
		if m.Title != "Departure gate changed" || !strings.Contains(m.Body, "B3") || strings.Contains(m.Body, "CALLBACK") {
			t.Fatalf("push %#v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback was not dispatched")
	}
	details, err := store.Details(ctx, prior.ID)
	if err != nil || details.Flight.DepartureGate != "B3" || len(details.Events) != 1 {
		t.Fatalf("details %#v (%v)", details, err)
	}
}

func TestSignedAdvisoryQueuesOnceAndDoesNotChangeFlightObservation(t *testing.T) {
	store, err := core.Open(filepath.Join(t.TempDir(), "advisory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	dep, arr := now.Add(25*time.Minute), now.Add(2*time.Hour)
	flight := feeds.Flight{ID: "a-test-1", Number: "UA88", Origin: "SFO", Destination: "EWR", DestinationICAO: "KEWR", Status: "scheduled", ObservedAt: now.Add(-time.Minute), ScheduledDeparture: &dep, ScheduledArrival: &arr}
	trackTestFlight(t, store, flight)
	key := []byte(strings.Repeat("b", 32))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := api.Server{Store: store, AdvisoryIngressKey: key, Logger: logger}.Handler()
	body := fmt.Sprintf(`{"version":1,"source":"faa_swim","advisory":{"id":"GS-1","airport":"KEWR","kind":"ground_stop","direction":"arrival","reason":"wind","observedAt":%q,"endsAt":%q}}`, now.Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
	call := func(at time.Time, signedBody, postedBody string) int {
		timestamp := strconv.FormatInt(at.Unix(), 10)
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(timestamp + "." + signedBody))
		r := httptest.NewRequest(http.MethodPost, "/v1/webhooks/advisory", strings.NewReader(postedBody))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Plano-Timestamp", timestamp)
		r.Header.Set("X-Plano-Signature", hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if code := call(now.Add(-10*time.Minute), body, body); code != 401 {
		t.Fatalf("expired signature status %d", code)
	}
	if code := call(now, body, strings.ReplaceAll(body, "wind", "snow")); code != 401 {
		t.Fatalf("tampered body status %d", code)
	}
	for range 2 {
		if code := call(now, body, body); code != 204 {
			t.Fatalf("callback status %d", code)
		}
	}
	details, err := store.Details(context.Background(), flight.ID)
	if err != nil {
		t.Fatal(err)
	}
	stops := 0
	for _, event := range details.Events {
		if event.Kind == "ground_stop" && event.Body == "wind" {
			stops++
		}
	}
	if stops != 1 || !details.Flight.ObservedAt.Equal(flight.ObservedAt) {
		t.Fatalf("details %#v", details)
	}
}
