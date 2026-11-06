package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
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
	inbound := feeds.Flight{ID: "in", Number: "UA7", Status: "airborne", ObservedAt: now}
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
		if err := store.Untrack(ctx, owner, parent.ID); err != nil {
			t.Fatal(err)
		}
	}
	get("/v1/flights/in?rotationOf=out", ownerToken, 404)
	get("/v1/flights/in?rotationOf=out", ownerToken, 404)
	if len(source.calls) != 3 {
		t.Fatalf("provider request was not fenced after untracking: %v", source.calls)
	}
}

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
