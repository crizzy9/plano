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
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/api"
	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

// BenchmarkSignedAdvisoryDispatch measures handler entry to the in-process
// sender with one flight, one device, and one request at a time. It includes
// the SQLite outbox commit and claim. Provider and device delivery are outside
// this measurement.
//
// GOMAXPROCS=2 go test -p=1 ./internal/api -run '^$' -bench '^BenchmarkSignedAdvisoryDispatch$' -benchtime=100x
func BenchmarkSignedAdvisoryDispatch(b *testing.B) {
	store, err := core.Open(filepath.Join(b.TempDir(), "latency.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(b.Context())
	defer cancel()
	now := time.Now().UTC()
	departure := now.Add(25 * time.Minute)
	flight := feeds.Flight{ID: "advisory-latency", Number: "UA88", Origin: "SFO", Destination: "EWR",
		DestinationICAO: "KEWR", Status: "scheduled", ObservedAt: now, ScheduledDeparture: &departure}
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		b.Fatal(err)
	}
	token, err := store.CreateInstallation(ctx)
	if err != nil {
		b.Fatal(err)
	}
	owner, err := store.Installation(ctx, token)
	if err != nil {
		b.Fatal(err)
	}
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		b.Fatal(err)
	}
	const deviceToken = "latency-fixture-device"
	if err := store.Register(ctx, owner, core.Device{Platform: "ios", Kind: "alert", Token: deviceToken}, now); err != nil {
		b.Fatal(err)
	}

	key := []byte(strings.Repeat("b", 32))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := api.Server{Store: store, AdvisoryIngressKey: key, Logger: logger}.Handler()
	type delivery struct {
		at      time.Time
		target  push.Target
		message push.Message
	}
	pushed := make(chan delivery, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.RunDispatcher(ctx, recordPush(func(ctx context.Context, target push.Target, message push.Message) error {
			select {
			case pushed <- delivery{at: time.Now(), target: target, message: message}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}), logger)
	}()
	defer func() { cancel(); <-done }()
	waitDelivery := func() delivery {
		select {
		case d := <-pushed:
			return d
		case <-time.After(3 * time.Second):
			b.Fatal("alert did not reach the fake sender")
			return delivery{}
		}
	}
	// Registration queues a check-in reminder for the fixture flight. Drain it
	// before sending the first advisory so it cannot be mistaken for a sample.
	if d := waitDelivery(); d.message.Title != "Check-in reminder" {
		b.Fatalf("unexpected initial delivery: %#v", d)
	}

	measure := func(index int) time.Duration {
		now := time.Now().UTC()
		reason := fmt.Sprintf("wind bulletin %d", index)
		body := fmt.Sprintf(`{"version":1,"source":"faa_swim","advisory":{"id":"latency-%d","airport":"KEWR","kind":"ground_stop","direction":"arrival","reason":%q,"observedAt":%q,"endsAt":%q}}`,
			index, reason, now.Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
		timestamp := strconv.FormatInt(now.Unix(), 10)
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(timestamp + "." + body))
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/webhooks/advisory", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Plano-Timestamp", timestamp)
		r.Header.Set("X-Plano-Signature", hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		start := time.Now()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			b.Fatalf("advisory status %d: %s", w.Code, w.Body)
		}
		d := waitDelivery()
		if d.target.Platform != "ios" || d.target.Token != deviceToken || d.message.Kind != push.Alert ||
			d.message.FlightID != flight.ID || d.message.Title != "KEWR · ground stop" || d.message.Body != reason {
			b.Fatalf("unexpected advisory delivery: %#v", d)
		}
		return d.at.Sub(start)
	}

	measure(0)
	var samples []time.Duration
	for b.Loop() {
		samples = append(samples, measure(len(samples)+1))
	}
	reportDispatchLatency(b, samples)
}

// BenchmarkSignedFlightObservationDispatch includes the SQLite write and
// dispatch claim. Its sender is a fixture and makes no provider requests.
func BenchmarkSignedFlightObservationDispatch(b *testing.B) {
	store, err := core.Open(filepath.Join(b.TempDir(), "flight-latency.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(b.Context())
	defer cancel()
	now := time.Now().UTC()
	departure, arrival := now.Add(30*time.Hour), now.Add(36*time.Hour)
	flight := feeds.Flight{ID: "UAL88-latency", Number: "UA88", Origin: "SFO", Destination: "EWR",
		OriginICAO: "KSFO", DestinationICAO: "KEWR", Status: "scheduled", Source: "flightaware_aeroapi",
		ObservedAt: now, ScheduledDeparture: &departure, ScheduledArrival: &arrival, DepartureGate: "B0"}
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		b.Fatal(err)
	}
	token, err := store.CreateInstallation(ctx)
	if err != nil {
		b.Fatal(err)
	}
	owner, err := store.Installation(ctx, token)
	if err != nil {
		b.Fatal(err)
	}
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		b.Fatal(err)
	}
	const deviceToken = "flight-latency-fixture"
	if err := store.Register(ctx, owner, core.Device{Platform: "ios", Kind: "alert", Token: deviceToken}, now); err != nil {
		b.Fatal(err)
	}
	key := []byte(strings.Repeat("f", 32))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := api.Server{Store: store, FlightObservationKey: key, Logger: logger}.Handler()
	type delivery struct {
		at      time.Time
		target  push.Target
		message push.Message
	}
	pushed := make(chan delivery, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.RunDispatcher(ctx, recordPush(func(ctx context.Context, target push.Target, message push.Message) error {
			select {
			case pushed <- delivery{time.Now(), target, message}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}), logger)
	}()
	defer func() { cancel(); <-done }()

	measure := func(index int) time.Duration {
		capture := time.Now().UTC()
		gate := fmt.Sprintf("B%d", index+1)
		body := flightObservationBody(flight.ID, gate, capture, departure, false)
		stamp := strconv.FormatInt(capture.Unix(), 10)
		request := flightObservationRequest(key, stamp, body, body).WithContext(ctx)
		response := httptest.NewRecorder()
		start := time.Now()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			b.Fatalf("flight observation returned %d: %s", response.Code, response.Body)
		}
		select {
		case sent := <-pushed:
			if sent.target.Platform != "ios" || sent.target.Token != deviceToken || sent.message.Kind != push.Alert ||
				sent.message.FlightID != flight.ID || sent.message.Title != "Departure gate changed" || sent.message.Body != "UA88 · "+gate {
				b.Fatalf("unexpected flight alert: %#v", sent)
			}
			return sent.at.Sub(start)
		case <-time.After(3 * time.Second):
			b.Fatal("flight alert did not reach the fake sender")
			return 0
		}
	}
	measure(0)
	var samples []time.Duration
	for b.Loop() {
		samples = append(samples, measure(len(samples)+1))
	}
	reportDispatchLatency(b, samples)
}

func reportDispatchLatency(b *testing.B, samples []time.Duration) {
	b.Helper()
	slices.Sort(samples)
	b.ReportMetric(float64(samples[(len(samples)-1)/2])/float64(time.Millisecond), "p50-dispatch-ms")
	b.ReportMetric(float64(samples[(95*len(samples)-1)/100])/float64(time.Millisecond), "p95-dispatch-ms")
}
