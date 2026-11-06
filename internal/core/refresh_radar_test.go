package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

type takeoffTrackSource struct {
	feeds.FlightSource
	flight feeds.Flight
	fix    feeds.Position
}

func (s takeoffTrackSource) Get(context.Context, string) (feeds.Flight, error) {
	return s.flight, nil
}

func (s takeoffTrackSource) Track(context.Context, string) ([]feeds.Position, error) {
	return []feeds.Position{s.fix}, nil
}

func TestRadarRefreshWithoutGateDeparture(t *testing.T) {
	for _, test := range []struct {
		status    string
		wheelsOff bool
	}{
		{status: "airborne"},
		{status: "diverted", wheelsOff: true},
	} {
		t.Run(test.status, func(t *testing.T) {
			ctx := t.Context()
			store, err := Open(filepath.Join(t.TempDir(), "plano.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			token, err := store.CreateInstallation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := store.Installation(ctx, token)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			takeoff, arrival := now.Add(-time.Hour), now.Add(time.Hour)
			flight := feeds.Flight{
				ID: "radar-takeoff", Number: "UA123", Origin: "SFO", Destination: "EWR", Status: test.status,
				ScheduledDeparture: &takeoff, ScheduledArrival: &arrival,
				ObservedAt: now.Add(-time.Second),
			}
			if test.wheelsOff {
				flight.ActualTakeoff = &takeoff
			}
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
				t.Fatal(err)
			}
			flight.ObservedAt = now
			fix := feeds.Position{At: now, Latitude: 40, Longitude: -105, Source: "flightaware_aeroapi"}
			refresher := Refresher{Store: store, Flights: takeoffTrackSource{flight: flight, fix: fix}, Logger: slog.New(slog.DiscardHandler)}
			if err := refresher.refresh(ctx); err != nil {
				t.Fatal(err)
			}
			positions, _, err := store.RadarProviderPositions(ctx, owner, flight.ID, nil, nil, 256)
			if err != nil || len(positions) != 1 || !positions[0].At.Equal(fix.At) {
				t.Fatalf("provider radar = %+v, error = %v", positions, err)
			}
		})
	}
}
