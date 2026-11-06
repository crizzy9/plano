package api

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestPickupMapOmitsInvalidFixes(t *testing.T) {
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	invalid := []feeds.Position{
		{Latitude: 40, Longitude: -74},
		{At: at, Latitude: math.NaN(), Longitude: -74},
		{At: at, Latitude: 40, Longitude: math.Inf(-1)},
		{At: at, Latitude: -91, Longitude: -74},
		{At: at, Latitude: 40, Longitude: 181},
	}
	if got := pickupMap(invalid); got != nil {
		t.Fatalf("invalid fixes produced a map: %+v", got)
	}
	got := pickupMap(append(invalid, feeds.Position{At: at, Latitude: 40, Longitude: -74}))
	if got == nil || got.Path != "" || !got.ObservedAt.Equal(at) ||
		got.Latest != (pickupMapPoint{X: 180, Y: 90}) || len(got.Dots) != 0 || got.Coastline == "" {
		t.Fatalf("unexpected single-fix map: %+v", got)
	}
}

func TestPickupMapUnwrapsDateLine(t *testing.T) {
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	got := pickupMap([]feeds.Position{
		{At: at, Latitude: 52, Longitude: 179},
		{At: at.Add(time.Minute), Latitude: 52, Longitude: -179},
	})
	if got == nil || !slices.Equal(got.Worlds, []int{-360, 0}) ||
		got.Latest.X <= 180 || got.Latest.X >= 220 || strings.Count(got.Path, "L") != 1 {
		t.Fatalf("date-line track should stay local and head east: %+v", got)
	}
}

func TestPickupMapLeavesObservationGaps(t *testing.T) {
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var positions []feeds.Position
	for _, minutes := range []int{0, 6, 11, 17, 18} {
		positions = append(positions, feeds.Position{At: at.Add(time.Duration(minutes) * time.Minute), Latitude: 40, Longitude: -74 + float64(minutes)/10})
	}
	got := pickupMap(positions)
	if got == nil || strings.Count(got.Path, "M") != 2 || strings.Count(got.Path, "L") != 2 || len(got.Dots) != 1 {
		t.Fatalf("expected two segments with one isolated fix: %+v", got)
	}
}

func TestPickupMapDrawsOnlyPointsForDisconnectedFixes(t *testing.T) {
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	got := pickupMap([]feeds.Position{
		{At: at, Latitude: 40, Longitude: -74},
		{At: at.Add(6 * time.Minute), Latitude: 40, Longitude: -73},
	})
	if got == nil || got.Path != "" || len(got.Dots) != 1 || got.Dots[0].X >= got.Latest.X {
		t.Fatalf("expected one prior dot and a separate latest fix: %+v", got)
	}
}

func TestPickupMapSamplesWithoutInventingGaps(t *testing.T) {
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	positions := make([]feeds.Position, 500)
	for i := range positions {
		positions[i] = feeds.Position{At: at.Add(time.Duration(i) * 4 * time.Minute), Latitude: 40, Longitude: -74 + float64(i)/100}
	}
	assertSegments := func(want int) {
		t.Helper()
		got := pickupMap(positions)
		if got == nil || strings.Count(got.Path, "M") != want || strings.Count(got.Path, "L") != pickupMapMaxFixes-want ||
			!got.ObservedAt.Equal(positions[len(positions)-1].At) || !strings.HasPrefix(got.Path, "M144.64 90.00") {
			t.Fatalf("expected %d fixes across %d segments with both endpoints: %+v", pickupMapMaxFixes, want, got)
		}
	}
	assertSegments(1)
	for i := len(positions) / 2; i < len(positions); i++ {
		positions[i].At = positions[i].At.Add(2 * time.Minute)
	}
	assertSegments(2)
}
