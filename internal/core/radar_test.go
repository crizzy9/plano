package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestRadarAppendCursorAndCurrentAircraftGuard(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	dep, arr := now.Add(-time.Hour), now.Add(time.Hour)
	flight := feeds.Flight{ID: "radar-fixture", Number: "XY123", Origin: "LAX", Destination: "EWR", TailNumber: "N123AA", ICAONumber: "XY123", Status: "airborne", ActualTakeoff: &dep, ScheduledDeparture: &dep, ScheduledArrival: &arr, ObservedAt: now}
	provider := feeds.Position{At: now.Add(-time.Minute), Latitude: 33, Longitude: -100, Source: "flightaware_aeroapi"}
	if err := s.Observe(ctx, flight, []feeds.Position{provider}, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	first := feeds.Position{At: now.Add(time.Nanosecond), Latitude: 35, Longitude: -92, Source: "operator_readsb"}
	second := feeds.Position{At: now.Add(2 * time.Nanosecond), Latitude: 35.01, Longitude: -91.99, Source: "operator_readsb"}
	if err := s.recordRadarSamples(ctx, []radarSample{{flight, first}, {flight, first}, {flight, second}}); err != nil {
		t.Fatal(err)
	}
	details, err := s.DetailsForInstallation(ctx, owner, flight.ID)
	if err != nil || !details.Flight.ObservedAt.Equal(now) || len(details.Telemetry) != 3 || len(details.Events) != 0 {
		t.Fatalf("details %#v: %v", details, err)
	}
	foreign, err := s.DetailsForInstallation(ctx, "unrelated", flight.ID)
	if err != nil || len(foreign.Telemetry) != 1 || foreign.Telemetry[0].Source != "flightaware_aeroapi" {
		t.Fatalf("provider/local isolation failed %#v %v", foreign.Telemetry, err)
	}
	foreignRadar, err := s.RadarPositions(ctx, "unrelated", flight.ID, nil, 256)
	if err != nil || len(foreignRadar) != 0 {
		t.Fatalf("foreign live radar %#v %v", foreignRadar, err)
	}
	positions, err := s.RadarPositions(ctx, owner, flight.ID, &first.At, 256)
	if err != nil || len(positions) != 1 || !positions[0].At.Equal(second.At) {
		t.Fatalf("cursor %#v: %v", positions, err)
	}
	newAircraft := flight
	newAircraft.TailNumber, newAircraft.ObservedAt = "N923BB", now.Add(time.Second)
	if err := s.Observe(ctx, newAircraft, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	wrong := feeds.Position{At: now.Add(3 * time.Second), Latitude: 37, Longitude: -89, Source: "operator_readsb"}
	if err := s.recordRadarSamples(ctx, []radarSample{{flight, wrong}}); err != nil {
		t.Fatal(err)
	}
	positions, err = s.RadarPositions(ctx, owner, flight.ID, nil, 256)
	if err != nil || len(positions) != 0 {
		t.Fatalf("wrong tail merged: %#v %v", positions, err)
	}
}

type radarFixtureTransport func(*http.Request) (*http.Response, error)

func (f radarFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRadarCycleSamplesOnlyTrackedMatchingReceiverFlight(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "radar-cycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	dep, arr := now.Add(-time.Hour), now.Add(time.Hour)
	tracked := feeds.Flight{ID: "sample-tracked", Number: "XY123", Origin: "LAX", Destination: "EWR", TailNumber: "N123AA", ICAONumber: "XY123", Status: "airborne", ActualTakeoff: &dep, ScheduledDeparture: &dep, ScheduledArrival: &arr, ObservedAt: now}
	untracked := tracked
	untracked.ID = "sample-untracked"
	for _, f := range []feeds.Flight{tracked, untracked} {
		if err := s.Observe(ctx, f, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, tracked.ID, ""); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: radarFixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/aircraft.json" {
			t.Fatalf("receiver request %s", r.URL.Path)
		}
		raw := fmt.Sprintf(`{"now":%d,"aircraft":[{"hex":"ab1234","r":"N123AA","flight":"XY123","type":"adsb_icao","lat":42,"lon":-72,"alt_baro":26000,"gs":410,"seen_pos":1}]}`, now.Unix())
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(raw))}, nil
	})}
	receiver, err := feeds.NewADSBReceiver("https://receiver.example.test/aircraft.json", client)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.sampleRadar(ctx, receiver, nil); err != nil {
		t.Fatal(err)
	}
	positions, err := s.RadarPositions(ctx, owner, tracked.ID, nil, 256)
	if err != nil || calls != 1 || len(positions) != 1 || positions[0].Latitude != 42 || positions[0].AltitudeFeet == nil || *positions[0].AltitudeFeet != 26000 {
		t.Fatalf("receiver fixture %#v calls=%d %v", positions, calls, err)
	}
	positions, err = s.RadarPositions(ctx, owner, untracked.ID, nil, 256)
	if err != nil || len(positions) != 0 {
		t.Fatalf("untracked points %#v %v", positions, err)
	}
}
