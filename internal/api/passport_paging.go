package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

const (
	maxPassportPageSize  = 100
	maxPassportJulianDay = 5_373_484.5 // SQLite's exclusive upper Julian day bound.
)

type passportHistoryPage struct {
	Flights    []passportHistoryFlight `json:"flights"`
	NextCursor string                  `json:"nextCursor,omitempty"`
}

type passportHistoryFlight struct {
	feeds.Flight
	HistoryKey string `json:"historyKey"`
}

type passportPagedResponse struct {
	passportHistoryPage
	passportSummary
}

func parsePassportPage(query url.Values) (int, *core.CompletedFlightCursor, error) {
	if !query.Has("limit") {
		if query.Has("cursor") {
			return 0, nil, errInvalid
		}
		return 0, nil, nil
	}
	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil || limit < 1 || limit > maxPassportPageSize || strconv.Itoa(limit) != query.Get("limit") {
		return 0, nil, errInvalid
	}
	if !query.Has("cursor") {
		return limit, nil, nil
	}
	encoded := query.Get("cursor")
	if encoded == "" || len(encoded) > 512 {
		return 0, nil, errInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, nil, errInvalid
	}
	var cursor struct {
		Arrival *float64
		ID      string
	}
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Arrival == nil || *cursor.Arrival < 0 || *cursor.Arrival >= maxPassportJulianDay || len(cursor.ID) < 3 {
		return 0, nil, errInvalid
	}
	if prefix := cursor.ID[:2]; (prefix != "l." && prefix != "p." && prefix != "a.") || !flightID.MatchString(cursor.ID[2:]) {
		return 0, nil, errInvalid
	}
	return limit, &core.CompletedFlightCursor{Arrival: *cursor.Arrival, ID: cursor.ID}, nil
}

func encodePassportCursor(cursor *core.CompletedFlightCursor) (string, error) {
	if cursor == nil {
		return "", nil
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

type passportRouteKey struct{ origin, destination passportAirport }

type passportRouteTotals struct {
	key                              passportRouteKey
	flights, miles, missing, example int
}

func (s Server) passportPage(ctx context.Context, w http.ResponseWriter, owner string, limit int, cursor *core.CompletedFlightCursor) error {
	if cursor != nil {
		records, next, err := s.Store.CompletedFlightPage(ctx, owner, cursor, limit)
		if err != nil {
			return err
		}
		encoded, err := encodePassportCursor(next)
		if err != nil {
			return err
		}
		flights := make([]passportHistoryFlight, 0, len(records))
		for _, row := range records {
			flights = append(flights, passportHistoryFlight{Flight: row.Flight, HistoryKey: row.Cursor.ID})
		}
		respond(w, http.StatusOK, passportHistoryPage{Flights: flights, NextCursor: encoded})
		return nil
	}
	value := passportPagedResponse{
		passportHistoryPage: passportHistoryPage{Flights: make([]passportHistoryFlight, 0, limit)},
		passportSummary:     passportSummary{Segments: []passportSegment{}},
	}
	aircraft := make(map[string]bool)
	routeIndexes := make(map[passportRouteKey]int)
	var routes []passportRouteTotals
	var airportFlights []feeds.Flight
	var last core.CompletedFlightCursor
	err := s.Store.WalkCompletedFlights(ctx, owner, func(flight feeds.Flight, position core.CompletedFlightCursor) {
		if len(value.Flights) < limit {
			value.Flights = append(value.Flights, passportHistoryFlight{Flight: flight, HistoryKey: position.ID})
			last = position
		}
		countPassportFlight(&value.passportSummary, aircraft, flight)
		key := passportRouteKey{passportAirportKey(flight.Origin, flight.OriginICAO), passportAirportKey(flight.Destination, flight.DestinationICAO)}
		index, exists := routeIndexes[key]
		if !exists {
			index = len(routes)
			routeIndexes[key] = index
			routes = append(routes, passportRouteTotals{key: key})
			airportFlights = append(airportFlights, feeds.Flight{Origin: flight.Origin, OriginICAO: flight.OriginICAO,
				Destination: flight.Destination, DestinationICAO: flight.DestinationICAO, HistoryDate: flight.HistoryDate, Source: flight.Source})
		} else {
			if flight.HistoryDate != "" {
				airportFlights[index].HistoryDate = flight.HistoryDate
			}
			if flight.Source != "flighty_csv" {
				airportFlights[index].Source = flight.Source
			}
		}
		route := &routes[index]
		route.flights++
		if distance := passportReportedDistance(flight); distance > 0 {
			route.miles += distance
			if route.example == 0 {
				route.example = distance
			}
		} else {
			route.missing++
		}
	})
	if err != nil {
		return err
	}
	if value.Stats.Flights > limit {
		value.NextCursor, err = encodePassportCursor(&last)
		if err != nil {
			return err
		}
	}
	coordinates, pending, err := s.passportCoordinates(ctx, airportFlights)
	if err != nil {
		return err
	}
	value.Coverage.PendingAirportLookups = pending
	addPassportRoutes(&value.passportSummary, routes, airportFlights, coordinates)
	value.Stats.Hours = math.Round(value.Stats.Hours*10) / 10
	value.Stats.Aircraft = len(aircraft)
	if err := ctx.Err(); err != nil {
		return err
	}
	respond(w, http.StatusOK, value)
	return nil
}

func addPassportRoutes(value *passportSummary, routes []passportRouteTotals, flights []feeds.Flight, points map[string]*core.AirportCoordinates) {
	mapped := make(map[[4]float64]bool)
	for index, route := range routes {
		value.Stats.Miles += route.miles
		origin, destination := points[route.key.origin.primary], points[route.key.destination.primary]
		fallback := 0
		if origin == nil || destination == nil {
			value.Coverage.MissingCoordinatesFlights += route.flights
		} else {
			fallback = greatCircle(*origin, *destination)
			key := [4]float64{origin.Latitude, origin.Longitude, destination.Latitude, destination.Longitude}
			if key[0] > key[2] || key[0] == key[2] && key[1] > key[3] {
				key = [4]float64{key[2], key[3], key[0], key[1]}
			}
			if !mapped[key] {
				flight := flights[index]
				distance := route.example
				if distance == 0 {
					distance = fallback
				}
				value.Segments = append(value.Segments, passportSegment{flight.Origin, flight.Destination,
					origin.Latitude, origin.Longitude, destination.Latitude, destination.Longitude, distance})
				mapped[key] = true
			}
		}
		value.Stats.Miles += fallback * route.missing
		if fallback == 0 {
			value.Coverage.MissingDistanceFlights += route.missing
		}
	}
}
