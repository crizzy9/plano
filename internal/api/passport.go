package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

const (
	passportLookupBatch     = 12
	airportMissingBackoff   = 24 * time.Hour
	airportTemporaryBackoff = 2 * time.Minute
)

var (
	passportAirportSlots = make(chan struct{}, 4)
	passportAirportCode  = regexp.MustCompile(`^[A-Z0-9]{2,7}$`)
)

type passportSegment struct {
	Origin               string  `json:"origin"`
	Destination          string  `json:"destination"`
	OriginLatitude       float64 `json:"originLatitude"`
	OriginLongitude      float64 `json:"originLongitude"`
	DestinationLatitude  float64 `json:"destinationLatitude"`
	DestinationLongitude float64 `json:"destinationLongitude"`
	DistanceMiles        int     `json:"distanceMiles"`
}

type passportStats struct {
	Flights      int     `json:"flights"`
	Miles        int     `json:"miles"`
	Hours        float64 `json:"hours"`
	Aircraft     int     `json:"aircraft"`
	DelayMinutes int     `json:"delayMinutes"`
}

type passportCoverage struct {
	MissingCoordinatesFlights int `json:"missingCoordinatesFlights"`
	MissingDistanceFlights    int `json:"missingDistanceFlights"`
	MissingDurationFlights    int `json:"missingDurationFlights"`
	MissingDelayFlights       int `json:"missingDelayFlights"`
	PendingAirportLookups     int `json:"pendingAirportLookups"`
}

type passportResponse struct {
	Flights  []feeds.Flight    `json:"flights"`
	Segments []passportSegment `json:"segments"`
	Stats    passportStats     `json:"stats"`
	Coverage passportCoverage  `json:"coverage"`
}

type passportAirport struct{ primary, alternate string }

func passportAirportKey(iata, icao string) passportAirport {
	iata, icao = strings.ToUpper(strings.TrimSpace(iata)), strings.ToUpper(strings.TrimSpace(icao))
	if !passportAirportCode.MatchString(iata) {
		iata = ""
	}
	if !passportAirportCode.MatchString(icao) || icao == iata {
		return passportAirport{primary: iata}
	}
	return passportAirport{primary: icao, alternate: iata}
}

var demoAirportCoordinates = map[string]core.AirportCoordinates{
	"SFO":  {Latitude: 37.62, Longitude: -122.38},
	"KSFO": {Latitude: 37.62, Longitude: -122.38},
	"EWR":  {Latitude: 40.69, Longitude: -74.17},
	"KEWR": {Latitude: 40.69, Longitude: -74.17},
	"LHR":  {Latitude: 51.47, Longitude: -0.45},
	"EGLL": {Latitude: 51.47, Longitude: -0.45},
}

func (s Server) passport(w http.ResponseWriter, r *http.Request, owner string) error {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	flights, err := s.Store.CompletedFlights(ctx, owner)
	if err != nil {
		return err
	}
	coordinates, pending, err := s.passportCoordinates(ctx, flights)
	if err != nil {
		return err
	}
	value := passportResponse{
		Flights: flights, Segments: make([]passportSegment, 0, len(flights)),
		Stats:    passportStats{Flights: len(flights)},
		Coverage: passportCoverage{PendingAirportLookups: pending},
	}
	aircraft := make(map[string]bool)
	for _, f := range flights {
		if tail := strings.ToUpper(strings.TrimSpace(f.TailNumber)); tail != "" {
			aircraft[tail] = true
		}
		if f.ActualDeparture != nil && !f.ActualDeparture.IsZero() && f.ActualArrival != nil && f.ActualArrival.After(*f.ActualDeparture) {
			value.Stats.Hours += f.ActualArrival.Sub(*f.ActualDeparture).Hours()
		} else {
			value.Coverage.MissingDurationFlights++
		}
		if f.ActualArrival == nil || f.ActualArrival.IsZero() || f.ScheduledArrival == nil || f.ScheduledArrival.IsZero() {
			value.Coverage.MissingDelayFlights++
		} else if f.ActualArrival.After(*f.ScheduledArrival) {
			value.Stats.DelayMinutes += int(f.ActualArrival.Sub(*f.ScheduledArrival).Minutes())
		}
		distance := 0
		if f.DistanceMiles != nil {
			distance = max(*f.DistanceMiles, 0)
		}
		origin := coordinates[passportAirportKey(f.Origin, f.OriginICAO).primary]
		destination := coordinates[passportAirportKey(f.Destination, f.DestinationICAO).primary]
		if origin != nil && destination != nil {
			if distance == 0 {
				distance = greatCircle(*origin, *destination)
			}
			value.Segments = append(value.Segments, passportSegment{
				f.Origin, f.Destination, origin.Latitude, origin.Longitude,
				destination.Latitude, destination.Longitude, distance,
			})
		} else {
			value.Coverage.MissingCoordinatesFlights++
		}
		if distance == 0 {
			value.Coverage.MissingDistanceFlights++
		}
		value.Stats.Miles += distance
	}
	value.Stats.Hours = math.Round(value.Stats.Hours*10) / 10
	value.Stats.Aircraft = len(aircraft)
	if err := ctx.Err(); err != nil {
		return err
	}
	respond(w, http.StatusOK, value)
	return nil
}

type passportAirportSource interface {
	Airport(context.Context, string) (feeds.Airport, error)
}

func (s Server) passportCoordinates(ctx context.Context, flights []feeds.Flight) (map[string]*core.AirportCoordinates, int, error) {
	points := make(map[string]*core.AirportCoordinates)
	if s.Demo {
		for code, point := range demoAirportCoordinates {
			points[code] = &point
		}
		return points, 0, nil
	}

	airports := make(map[string]passportAirport)
	var codes []string
	seen := make(map[string]bool)
	for _, flight := range flights {
		for _, airport := range []passportAirport{
			passportAirportKey(flight.Origin, flight.OriginICAO),
			passportAirportKey(flight.Destination, flight.DestinationICAO),
		} {
			if airport.primary == "" {
				continue
			}
			if current, ok := airports[airport.primary]; !ok || current.alternate == "" {
				airports[airport.primary] = airport
			}
			for _, code := range []string{airport.primary, airport.alternate} {
				if code != "" && !seen[code] {
					codes = append(codes, code)
					seen[code] = true
				}
			}
		}
	}
	cached, err := s.Store.ReadAirportCoordinates(ctx, codes)
	if err != nil {
		return nil, 0, err
	}
	now := time.Now()
	var candidates []passportAirport
	for code, airport := range airports {
		point := cached[code].Coordinates
		if point == nil {
			point = cached[airport.alternate].Coordinates
		}
		if point != nil {
			points[code] = point
		} else if !cached[code].RetryAfter.After(now) {
			candidates = append(candidates, airport)
		}
	}
	provider, ok := s.Source.(passportAirportSource)
	if !ok || len(candidates) == 0 {
		return points, 0, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].primary < candidates[j].primary })
	attempted := fetchPassportCoordinates(ctx, provider, candidates[:min(len(candidates), passportLookupBatch)])
	if err := s.Store.SaveAirportCoordinates(ctx, attempted); err != nil {
		return nil, 0, err
	}
	for code, entry := range attempted {
		if entry.Coordinates != nil {
			points[code] = entry.Coordinates
		}
	}
	return points, len(candidates) - len(attempted), nil
}

// Fetch at most one bounded batch per request. The shared slots also cap
// provider calls across concurrent Passport requests, and buffering lets an
// expired HTTP request return without waiting on a provider that is still busy.
func fetchPassportCoordinates(ctx context.Context, provider passportAirportSource, candidates []passportAirport) map[string]core.AirportCacheEntry {
	budget, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	type result struct {
		code  string
		entry core.AirportCacheEntry
	}
	results := make(chan result, len(candidates))
	for _, airport := range candidates {
		go func() {
			select {
			case passportAirportSlots <- struct{}{}:
				defer func() { <-passportAirportSlots }()
			case <-budget.Done():
				return
			}
			if budget.Err() != nil {
				return
			}
			deadline, cancel := context.WithTimeout(budget, 1200*time.Millisecond)
			defer cancel()
			entry := fetchPassportAirport(deadline, provider, airport)
			results <- result{airport.primary, entry}
		}()
	}
	entries := make(map[string]core.AirportCacheEntry, len(candidates))
	for range candidates {
		select {
		case received := <-results:
			entries[received.code] = received.entry
		case <-budget.Done():
			return entries
		}
	}
	return entries
}

func fetchPassportAirport(ctx context.Context, provider passportAirportSource, key passportAirport) core.AirportCacheEntry {
	entry, retry := findPassportPoint(ctx, provider, key.primary)
	if entry == nil && retry == airportMissingBackoff && key.alternate != "" && ctx.Err() == nil {
		entry, retry = findPassportPoint(ctx, provider, key.alternate)
	}
	if entry != nil {
		return core.AirportCacheEntry{Coordinates: entry}
	}
	return core.AirportCacheEntry{RetryAfter: time.Now().Add(retry)}
}

func findPassportPoint(ctx context.Context, provider passportAirportSource, code string) (*core.AirportCoordinates, time.Duration) {
	airport, err := provider.Airport(ctx, code)
	if err != nil && !errors.Is(err, feeds.ErrNotFound) && !errors.Is(err, feeds.ErrInvalidQuery) {
		return nil, airportTemporaryBackoff
	}
	if err == nil && airport.Latitude != nil && airport.Longitude != nil {
		lat, lon := *airport.Latitude, *airport.Longitude
		if lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 {
			return &core.AirportCoordinates{Latitude: lat, Longitude: lon}, 0
		}
	}
	return nil, airportMissingBackoff
}

func greatCircle(from, to core.AirportCoordinates) int {
	deg := math.Pi / 180
	lat, lon := (to.Latitude-from.Latitude)*deg, (to.Longitude-from.Longitude)*deg
	value := math.Pow(math.Sin(lat/2), 2) + math.Cos(from.Latitude*deg)*math.Cos(to.Latitude*deg)*math.Pow(math.Sin(lon/2), 2)
	return int(math.Round(2 * 3958.76 * math.Asin(math.Sqrt(min(1, max(0, value))))))
}
