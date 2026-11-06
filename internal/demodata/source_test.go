package demodata

import (
	"reflect"
	"testing"
	"time"
)

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
