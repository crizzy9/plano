package demodata

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestDemoRotationUsesSameDayInbound(t *testing.T) {
	source := Source{}
	for _, date := range []string{"2026-11-06", "2028-02-29"} {
		t.Run(date, func(t *testing.T) {
			outbound, err := source.Get(t.Context(), "demo-UA456-"+date)
			if err != nil {
				t.Fatal(err)
			}
			before := time.Now().UTC()
			rotation, err := source.Rotation(t.Context(), outbound)
			after := time.Now().UTC()
			if err != nil || len(rotation) != 1 {
				t.Fatalf("rotation has %d legs, want one: %v", len(rotation), err)
			}
			inbound := rotation[0]
			day, err := time.Parse(time.DateOnly, date)
			if err != nil {
				t.Fatal(err)
			}
			if inbound.ID != "demo-UA123-"+date || inbound.Number != "UA123" ||
				inbound.Origin != "SFO" || inbound.Destination != outbound.Origin ||
				inbound.TailNumber != outbound.TailNumber || inbound.Source != demoSource {
				t.Fatalf("unexpected inbound identity or aircraft: %+v", inbound)
			}
			if inbound.ScheduledDeparture == nil || !inbound.ScheduledDeparture.Equal(day.Add(15*time.Hour)) ||
				inbound.ScheduledArrival == nil || !inbound.ScheduledArrival.Equal(day.Add(20*time.Hour)) ||
				outbound.ScheduledDeparture.Sub(*inbound.ScheduledArrival) != 2*time.Hour {
				t.Fatalf("unexpected inbound chronology: %+v", inbound)
			}
			if inbound.ObservedAt.Before(before) || inbound.ObservedAt.After(after) {
				t.Fatalf("inbound observation is outside this provider call: %s", inbound.ObservedAt)
			}
			for _, actual := range []*time.Time{inbound.ActualDeparture, inbound.ActualTakeoff, inbound.ActualLanding, inbound.ActualArrival} {
				if actual != nil && actual.After(inbound.ObservedAt) {
					t.Fatalf("inbound contains a future actual movement: %s", actual)
				}
			}
		})
	}
}

func TestDemoRotationRejectsUnsupportedIdentity(t *testing.T) {
	source := Source{}
	for _, id := range []string{"unknown", "demo-UA789-2026-11-06", "demo-UA456-", "demo-UA456-2026-02-30"} {
		rotation, err := source.Rotation(context.Background(), feeds.Flight{ID: id})
		if !errors.Is(err, feeds.ErrNotFound) || len(rotation) != 0 {
			t.Errorf("rotation for %q returned %d legs and %v", id, len(rotation), err)
		}
	}
	rotation, err := source.Rotation(t.Context(), feeds.Flight{ID: "demo-UA123-2026-11-06"})
	if err != nil || len(rotation) != 0 {
		t.Fatalf("UA123 has an unexpected predecessor: %+v, %v", rotation, err)
	}
}

func TestDemoTrackOnlyShowsElapsedFlight(t *testing.T) {
	day := time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		id         string
		takeoff    time.Time
		landing    time.Time
		originLon  float64
		arrivalLon float64
	}{
		{"demo-UA123-2026-11-06", day.Add(15*time.Hour + 12*time.Minute), day.Add(19*time.Hour + 50*time.Minute), -122.38, -74.17},
		{"demo-UA456-2026-11-06", day.Add(22*time.Hour + 12*time.Minute), day.Add(28*time.Hour + 50*time.Minute), -74.17, -0.45},
	} {
		t.Run(test.id, func(t *testing.T) {
			before, err := trackAt(test.id, test.takeoff.Add(-time.Second))
			if err != nil || len(before) != 0 {
				t.Fatalf("track before takeoff has %d points: %v", len(before), err)
			}
			inFlight := test.takeoff.Add(29*time.Minute + 59*time.Second)
			track, err := trackAt(test.id, inFlight)
			if err != nil || len(track) != 30 || !track[29].At.Equal(test.takeoff.Add(29*time.Minute)) {
				t.Fatalf("partial track has %d points: %v", len(track), err)
			}
			complete, err := trackAt(test.id, test.landing.Add(48*time.Hour))
			wantCount := 1 + int(test.landing.Sub(test.takeoff)/time.Minute)
			if err != nil || len(complete) != wantCount || !reflect.DeepEqual(track, complete[:len(track)]) {
				t.Fatalf("completed track has %d points, want %d and the same prefix: %v", len(complete), wantCount, err)
			}
			first, last := complete[0], complete[len(complete)-1]
			if !first.At.Equal(test.takeoff) || !last.At.Equal(test.landing) || first.Longitude != test.originLon ||
				last.Longitude < test.arrivalLon-0.001 || last.Longitude > test.arrivalLon+0.001 {
				t.Fatalf("unexpected route endpoints: %+v to %+v", first, last)
			}
			for index, position := range complete {
				if position.Source != demoSource || position.Kind != "synthetic" || position.AltitudeFeet == nil ||
					position.GroundSpeedKnots == nil || position.VerticalRateFPM == nil {
					t.Fatalf("unlabeled or incomplete synthetic point %d: %+v", index, position)
				}
			}
			if *first.VerticalRateFPM <= 0 || *track[29].VerticalRateFPM != 0 || *last.VerticalRateFPM >= 0 {
				t.Fatal("expected climb, cruise, and descent")
			}
		})
	}
}
