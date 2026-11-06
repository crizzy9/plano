package itinerary

import (
	_ "embed"
	"strings"
	"sync"
	"time"
)

//go:embed passport_airport_zones.csv
var passportAirportZonesCSV string

var passportAirportLocations = sync.OnceValue(func() map[string]*time.Location {
	locations := make(map[string]*time.Location)
	byZone := make(map[string]*time.Location)
	for line := range strings.SplitSeq(passportAirportZonesCSV, "\n") {
		code, zone, ok := strings.Cut(line, ",")
		if !ok {
			continue
		}
		location, loaded := byZone[zone]
		if !loaded {
			location, _ = time.LoadLocation(zone)
			byZone[zone] = location
		}
		if location != nil {
			locations[code] = location
		}
	}
	return locations
})

func passportAirportLocation(code string) *time.Location {
	return passportAirportLocations()[code]
}
