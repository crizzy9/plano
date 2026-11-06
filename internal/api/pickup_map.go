package api

import (
	_ "embed"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const (
	pickupMapWidth    = 360
	pickupMapHeight   = 180
	pickupMapInset    = 16
	pickupMapMaxInput = 2_000
	pickupMapMaxFixes = 256
	pickupMapGap      = 5 * time.Minute
)

// Public-domain Natural Earth 1:110m land, v5.1.2. This path uses the same
// simplified exterior rings as mobile/src/passport-land.tsx.
//
//go:embed pickup_land.path
var pickupCoastline string

type pickupMapPoint struct{ X, Y float64 }

type pickupTrackMap struct {
	Coastline, LandTransform, Path string
	Worlds                         []int
	Dots                           []pickupMapPoint
	Latest                         pickupMapPoint
	ObservedAt                     time.Time
}

func pickupMap(positions []feeds.Position) *pickupTrackMap {
	// The store supplies its most recent fixes in chronological order.
	if len(positions) > pickupMapMaxInput {
		positions = positions[len(positions)-pickupMapMaxInput:]
	}
	type sample struct {
		pickupMapPoint
		at      time.Time
		segment int
	}
	samples := make([]sample, 0, len(positions))
	for _, p := range positions {
		if p.At.IsZero() || math.IsNaN(p.Latitude) || math.IsNaN(p.Longitude) ||
			math.Abs(p.Latitude) > 90 || math.Abs(p.Longitude) > 180 {
			continue
		}
		samples = append(samples, sample{pickupMapPoint: pickupMapPoint{X: p.Longitude, Y: -p.Latitude}, at: p.At})
	}
	if len(samples) == 0 {
		return nil
	}
	slices.SortStableFunc(samples, func(a, b sample) int { return a.at.Compare(b.at) })
	samples = slices.CompactFunc(samples, func(a, b sample) bool { return a.at.Equal(b.at) })

	latest := samples[len(samples)-1]
	west, east, north, south := latest.X, latest.X, latest.Y, latest.Y
	for i := len(samples) - 2; i >= 0; i-- {
		p := &samples[i]
		// Use the world copy closest to the next point when crossing the date line.
		p.X += 360 * math.Round((samples[i+1].X-p.X)/360)
		nextWest, nextEast := min(west, p.X), max(east, p.X)
		if nextEast-nextWest > 360 {
			// A corrupt track can wrap repeatedly. Keep the newest single-world span.
			samples = samples[i+1:]
			break
		}
		west, east = nextWest, nextEast
		north, south = min(north, p.Y), max(south, p.Y)
	}
	centerX, centerY := (west+east)/2, (north+south)/2
	longitudeScale := max(0.15, math.Cos(centerY*math.Pi/180))
	scaleY := min((pickupMapWidth-2*pickupMapInset)/max((east-west)*longitudeScale, 1),
		(pickupMapHeight-2*pickupMapInset)/max(south-north, 8))
	scaleX := scaleY * longitudeScale
	offsetX, offsetY := pickupMapWidth/2-centerX*scaleX, pickupMapHeight/2-centerY*scaleY
	result := &pickupTrackMap{
		Coastline:     strings.TrimSpace(pickupCoastline),
		LandTransform: fmt.Sprintf("matrix(%.8f 0 0 %.8f %.8f %.8f)", scaleX, scaleY, offsetX, offsetY),
		ObservedAt:    latest.at.UTC(),
	}
	firstWorld := int(math.Ceil((-offsetX/scaleX - 180) / 360))
	lastWorld := int(math.Floor(((pickupMapWidth-offsetX)/scaleX + 180) / 360))
	for world := firstWorld; world <= lastWorld; world++ {
		result.Worlds = append(result.Worlds, 360*world)
	}

	// Detect gaps on the original fixes so thinning a dense run can't split it.
	segment := 0
	for i := 1; i < len(samples); i++ {
		if samples[i].at.Sub(samples[i-1].at) > pickupMapGap {
			segment++
		}
		samples[i].segment = segment
	}
	if len(samples) > pickupMapMaxFixes {
		last := len(samples) - 1
		for i := range pickupMapMaxFixes {
			samples[i] = samples[i*last/(pickupMapMaxFixes-1)]
		}
		samples = samples[:pickupMapMaxFixes]
	}

	var path strings.Builder
	for i, p := range samples {
		point := pickupMapPoint{X: p.X*scaleX + offsetX, Y: p.Y*scaleY + offsetY}
		result.Latest = point
		command := 'L'
		if i == 0 || p.segment != samples[i-1].segment {
			if i+1 == len(samples) {
				continue // The latest fix already has its own marker.
			}
			if samples[i+1].segment != p.segment {
				result.Dots = append(result.Dots, point)
				continue
			}
			command = 'M'
		}
		fmt.Fprintf(&path, "%c%.2f %.2f", command, point.X, point.Y)
	}
	result.Path = path.String()
	return result
}
