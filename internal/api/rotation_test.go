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
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestTrackedRotationLegDetailsAndProviderTrack(t *testing.T) {
	ctx := t.Context()
	store, err := core.Open(filepath.Join(t.TempDir(), "rotation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().Add(-time.Minute)
	parent := feeds.Flight{ID: "out", ObservedAt: now}
	inbound := feeds.Flight{ID: "in", Number: "UA7", Origin: "BOS", OriginICAO: "KBOS", Status: "airborne", TailNumber: "N1AA", ObservedAt: now,
		ActualTakeoff: new(now.Add(-time.Hour))}
	earlier := feeds.Flight{ID: "earlier", Number: "UA8", Status: "arrived", ObservedAt: now}
	if err := store.Observe(ctx, parent, nil, []feeds.Flight{inbound, earlier}, nil); err != nil {
		t.Fatal(err)
	}
	ownerToken, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, ownerToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, parent.ID, ""); err != nil {
		t.Fatal(err)
	}
	source := &rotationTrackFixture{points: make([]feeds.Position, 300)}
	for i := range source.points {
		source.points[i] = feeds.Position{Latitude: 38, Longitude: -121, At: now.Add(time.Duration(i) * time.Millisecond)}
	}
	handler := Server{Store: store, Source: source, Logger: slog.New(slog.DiscardHandler)}.Handler()
	get := func(path, token string, want int) []byte {
		t.Helper()
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("GET %s = %d %s, want %d", path, response.Code, response.Body, want)
		}
		return response.Body.Bytes()
	}
	get("/v1/flights/in", ownerToken, 404)
	get("/v1/flights/in?rotationOf=out", otherToken, 404)
	get("/v1/flights/missing?rotationOf=out", ownerToken, 404)
	get("/v1/flights/earlier?rotationOf=out", ownerToken, 200)
	if len(source.calls) != 0 {
		t.Fatalf("unexpected provider calls: %v", source.calls)
	}
	var result struct {
		core.Details
		RotationSnapshot bool `json:"rotationSnapshot"`
	}
	if err := json.Unmarshal(get("/v1/flights/in?rotationOf=out", ownerToken, 200), &result); err != nil {
		t.Fatal(err)
	}
	if !result.RotationSnapshot || result.Tracked || result.Flight.ID != inbound.ID || len(result.Telemetry) != 256 ||
		!result.Telemetry[0].At.Equal(source.points[44].At) || len(source.calls) != 1 || source.calls[0] != inbound.ID {
		t.Fatalf("unexpected direct inbound result: flight %s, points %d, calls %v", result.Flight.ID, len(result.Telemetry), source.calls)
	}
	source.err = feeds.ErrUnavailable
	result.Telemetry = nil
	if err := json.Unmarshal(get("/v1/flights/in?rotationOf=out", ownerToken, 200), &result); err != nil ||
		!result.RotationSnapshot || len(result.Telemetry) != 0 {
		t.Fatalf("provider track outage should still return the saved leg: %+v %v", result, err)
	}
	source.err = nil
	source.beforeReturn = func() {
		parent.ObservedAt = parent.ObservedAt.Add(time.Millisecond)
		inbound.ObservedAt = parent.ObservedAt
		inbound.EstimatedArrival = new(now.Add(time.Hour))
		inbound.Origin = inbound.OriginICAO
		if err := store.Observe(ctx, parent, nil, []feeds.Flight{inbound, earlier}, nil); err != nil {
			t.Fatal(err)
		}
	}
	result.Telemetry = nil
	if err := json.Unmarshal(get("/v1/flights/in?rotationOf=out", ownerToken, 200), &result); err != nil ||
		len(result.Telemetry) != 256 || result.Flight.EstimatedArrival == nil || result.Flight.Origin != inbound.Origin {
		t.Fatalf("an estimate and display refresh dropped matching telemetry: %+v %v", result.Flight, err)
	}
	source.beforeReturn = func() {
		parent.ObservedAt = parent.ObservedAt.Add(time.Millisecond)
		inbound.ObservedAt = parent.ObservedAt
		inbound.Source = "replacement-feed"
		if err := store.Observe(ctx, parent, nil, []feeds.Flight{inbound, earlier}, nil); err != nil {
			t.Fatal(err)
		}
	}
	result.Telemetry = nil
	if err := json.Unmarshal(get("/v1/flights/in?rotationOf=out", ownerToken, 200), &result); err != nil ||
		result.Flight.Source != inbound.Source || len(result.Telemetry) != 0 {
		t.Fatalf("replacement feed got old inbound telemetry: %+v %v", result, err)
	}
	source.beforeReturn = func() {
		parent.ObservedAt = parent.ObservedAt.Add(time.Millisecond)
		inbound.ObservedAt = parent.ObservedAt
		inbound.TailNumber = "N2BB"
		inbound.ActualTakeoff = new(now.Add(-30 * time.Minute))
		if err := store.Observe(ctx, parent, nil, []feeds.Flight{inbound, earlier}, nil); err != nil {
			t.Fatal(err)
		}
	}
	result.Telemetry = nil
	if err := json.Unmarshal(get("/v1/flights/in?rotationOf=out", ownerToken, 200), &result); err != nil ||
		result.Flight.TailNumber != inbound.TailNumber || len(result.Telemetry) != 0 {
		t.Fatalf("replacement aircraft got old inbound telemetry: %+v %v", result, err)
	}
	source.beforeReturn = func() {
		if err := store.Untrack(ctx, owner, parent.ID); err != nil {
			t.Fatal(err)
		}
	}
	get("/v1/flights/in?rotationOf=out", ownerToken, 404)
	get("/v1/flights/in?rotationOf=out", ownerToken, 404)
	if len(source.calls) != 6 {
		t.Fatalf("provider request was not fenced after untracking: %v", source.calls)
	}
}

func TestTrackedRotationReceiverTrackDuringProviderOutage(t *testing.T) {
	ctx := t.Context()
	store, err := core.Open(filepath.Join(t.TempDir(), "rotation-receiver.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	parent := feeds.Flight{ID: "out", Number: "UA8", Origin: "EWR", Destination: "SFO",
		Status: "scheduled", TailNumber: "N1AA", ObservedAt: now, ScheduledDeparture: new(now.Add(2 * time.Hour))}
	inbound := feeds.Flight{ID: "in", Number: "UA7", Origin: "BOS", OriginICAO: "KBOS", Destination: "EWR",
		Source: "flightaware_aeroapi", Status: "airborne", TailNumber: "N1AA", ObservedAt: now,
		ScheduledDeparture: new(now.Add(-2 * time.Hour)), ActualTakeoff: new(now.Add(-time.Hour)),
		ScheduledArrival: new(now.Add(time.Hour))}
	if err := store.Observe(ctx, parent, nil, []feeds.Flight{inbound}, nil); err != nil {
		t.Fatal(err)
	}
	token, owner := newImportTestInstallation(t, store)
	other, _ := newImportTestInstallation(t, store)
	if err := store.Track(ctx, owner, parent.ID, ""); err != nil {
		t.Fatal(err)
	}

	// The second request starts only after the first cycle has finished writing.
	nextCycle, stopped := make(chan struct{}), make(chan struct{})
	calls := 0
	receiver, err := feeds.NewADSBReceiver("https://receiver.example.test/aircraft.json", &http.Client{
		Transport: rotationReceiverTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls > 1 {
				if calls == 2 {
					close(nextCycle)
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			raw := fmt.Sprintf(`{"now":%d,"aircraft":[{"hex":"ab1234","r":"N1AA","flight":"UA7","type":"adsb_icao","lat":42,"lon":-72,"seen_pos":1}]}`, now.Unix())
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	workerCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer close(stopped)
		store.RunRadar(workerCtx, receiver, nil, 5*time.Millisecond, logger)
	}()
	select {
	case <-nextCycle:
		cancel()
		<-stopped
	case <-time.After(3 * time.Second):
		cancel()
		<-stopped
		t.Fatal("receiver worker did not complete a sample cycle")
	}

	at := now.Add(-time.Second)
	source := &rotationTrackFixture{points: []feeds.Position{
		{At: at.Add(-time.Second), Latitude: 41, Longitude: -72, Source: "flightaware_aeroapi"},
	}}
	server := Server{Store: store, Source: source, Logger: logger}
	load := func() core.Details {
		t.Helper()
		raw := importTestRequest(t, server.Handler(), "GET", "/v1/flights/in?rotationOf=out", token, "", 200)
		var result core.Details
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	points := load().Telemetry
	if len(points) != 2 || !points[0].At.Before(points[1].At) || points[1].Source != "operator_readsb" || !points[1].At.Equal(at) {
		t.Fatalf("expected chronological provider and receiver positions, got %+v", points)
	}
	source.points = append(source.points, points[1])
	if points := load().Telemetry; len(points) != 2 {
		t.Fatalf("expected exact provider and receiver duplicates to collapse, got %+v", points)
	}
	rewriteInbound := func() {
		parent.ObservedAt = parent.ObservedAt.Add(time.Millisecond)
		inbound.ObservedAt = parent.ObservedAt
		if err := store.Observe(ctx, parent, nil, []feeds.Flight{inbound}, nil); err != nil {
			t.Fatal(err)
		}
	}
	source.beforeReturn = func() {
		inbound.ScheduledDeparture = new(inbound.ScheduledDeparture.Add(30 * time.Minute))
		rewriteInbound()
	}
	if result := load(); result.Flight.ScheduledDeparture == nil || !result.Flight.ScheduledDeparture.Equal(*inbound.ScheduledDeparture) ||
		len(result.Telemetry) != 1 || result.Telemetry[0].Source != "operator_readsb" || !result.Telemetry[0].At.Equal(at) {
		t.Fatalf("a schedule correction should retain only the receiver fix: %+v", result)
	}
	source.beforeReturn = nil
	source.err = feeds.ErrUnavailable
	if points := load().Telemetry; len(points) != 1 || points[0].Source != "operator_readsb" {
		t.Fatalf("expected receiver position during provider outage, got %+v", points)
	}
	server.Source = nil
	if points := load().Telemetry; len(points) != 1 || points[0].Source != "operator_readsb" {
		t.Fatalf("expected receiver position without a provider, got %+v", points)
	}
	server.Source = source
	source.err = nil
	source.beforeReturn = func() {
		inbound.Origin, inbound.OriginICAO = "PVD", "KPVD"
		rewriteInbound()
	}
	if result := load(); result.Flight.Origin != inbound.Origin || result.Flight.OriginICAO != inbound.OriginICAO || len(result.Telemetry) != 0 {
		t.Fatalf("a route correction should discard the old provider and receiver fixes: %+v", result)
	}
	importTestRequest(t, server.Handler(), "GET", "/v1/flights/in?rotationOf=out", other, "", 404)
}

type rotationReceiverTransport func(*http.Request) (*http.Response, error)

func (f rotationReceiverTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type rotationTrackFixture struct {
	feeds.FlightSource
	points       []feeds.Position
	calls        []string
	err          error
	beforeReturn func()
}

func (s *rotationTrackFixture) Track(_ context.Context, id string) ([]feeds.Position, error) {
	s.calls = append(s.calls, id)
	if s.beforeReturn != nil {
		s.beforeReturn()
	}
	return s.points, s.err
}
