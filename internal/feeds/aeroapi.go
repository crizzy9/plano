package feeds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // Retain departure-local date matching in minimal containers.
)

const (
	aeroBase       = "https://aeroapi.flightaware.com/aeroapi"
	aeroSource     = "flightaware_aeroapi"
	rotationWindow = 25 * time.Hour
)

var (
	designator         = regexp.MustCompile(`^[A-Z0-9]{2,3}[0-9]{1,5}[A-Z]?$\z`)
	providerIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$\z`)
)

type AeroAPI struct {
	key      string
	base     string
	client   *http.Client
	now      func() time.Time
	registry *FAARegistry
}

func NewAeroAPI(apiKey string, client *http.Client) *AeroAPI {
	return &AeroAPI{key: strings.TrimSpace(apiKey), base: aeroBase, client: feedClient(client), now: time.Now}
}

// WithFAARegistry returns a configured copy that shares the HTTP client.
func (a *AeroAPI) WithFAARegistry(registry *FAARegistry) *AeroAPI {
	configured := *a
	configured.registry = registry
	return &configured
}

// Search returns flights departing on the requested calendar day at their origin.
func (a *AeroAPI) Search(ctx context.Context, number string, date time.Time) ([]Flight, error) {
	number = strings.ToUpper(strings.Join(strings.Fields(number), ""))
	if !designator.MatchString(number) || date.IsZero() {
		return nil, ErrInvalidQuery
	}
	year, month, day := date.Date()
	midnight := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	start := midnight.Add(-14 * time.Hour)
	end := midnight.Add(36 * time.Hour)
	params := url.Values{"ident_type": {"designator"}, "max_pages": {"3"}}
	now := a.now().UTC()
	path := "/flights/"
	switch {
	case end.Before(now.Add(-10 * 24 * time.Hour)):
		path = "/history/flights/"
		params.Set("start", start.Format(time.RFC3339))
		params.Set("end", end.Format(time.RFC3339))
	case !start.Before(now.Add(48 * time.Hour)):
		return a.future(ctx, number, midnight, start, end)
	default:
		// Let the provider use its defaults outside its live endpoint window.
		if !start.Before(now.Add(-10 * 24 * time.Hour)) {
			params.Set("start", start.Format(time.RFC3339))
		}
		if end.Before(now.Add(48 * time.Hour)) {
			params.Set("end", end.Format(time.RFC3339))
		}
	}
	rows, err := a.flightRows(ctx, path+number, params)
	if errors.Is(err, ErrNotFound) {
		return []Flight{}, nil
	}
	if err != nil {
		return nil, err
	}
	found := make([]Flight, 0, len(rows))
	seen := make(map[string]int)
	for _, row := range rows {
		departure := row.ScheduledOut
		if departure == nil {
			departure = row.ScheduledOff
		}
		if row.Blocked || !providerIdentifier.MatchString(row.ID) || departure == nil || !onDepartureDate(*departure, row.Origin.Timezone, midnight) {
			continue
		}
		flight := a.normalizeFlight(row, now)
		if i, ok := seen[flight.ID]; ok {
			// The original row conveys the diversion for shared provider IDs.
			if row.Diverted {
				found[i] = flight
			}
			continue
		}
		seen[flight.ID] = len(found)
		found = append(found, flight)
	}
	sort.SliceStable(found, func(i, j int) bool {
		return found[i].ScheduledDeparture.Before(*found[j].ScheduledDeparture)
	})
	if len(found) == 0 && midnight.Add(24*time.Hour).After(now.Add(24*time.Hour)) {
		return a.future(ctx, number, midnight, start, end)
	}
	return found, nil
}

func onDepartureDate(instant time.Time, timezone string, date time.Time) bool {
	instant = instant.UTC()
	if timezone != "" {
		if local, err := time.LoadLocation(timezone); err == nil {
			instant = instant.In(local)
		}
	}
	return instant.Format(time.DateOnly) == date.Format(time.DateOnly)
}

func (a *AeroAPI) Get(ctx context.Context, id string) (Flight, error) {
	if strings.HasPrefix(id, schedulePrefix) {
		return a.getScheduled(ctx, id)
	}
	return a.getProviderFlight(ctx, id)
}

func (a *AeroAPI) getProviderFlight(ctx context.Context, id string) (Flight, error) {
	if !providerIdentifier.MatchString(id) {
		return Flight{}, ErrInvalidQuery
	}
	path := "/flights/" + id
	params := url.Values{"ident_type": {"fa_flight_id"}}
	now := a.now().UTC()
	rows, err := a.flightRows(ctx, path, params)
	if errors.Is(err, ErrNotFound) {
		rows, err = a.flightRows(ctx, "/history"+path, params)
	}
	if err != nil {
		return Flight{}, err
	}
	var original *aeroFlight
	for i := range rows {
		row := &rows[i]
		if row.ID != id || row.Blocked {
			continue
		}
		if original == nil || row.Diverted {
			original = row
		}
	}
	if original == nil {
		return Flight{}, ErrNotFound
	}
	return a.normalizeFlight(*original, now), nil
}

func (a *AeroAPI) Track(ctx context.Context, id string) ([]Position, error) {
	if strings.HasPrefix(id, schedulePrefix) {
		row, err := a.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if row.providerID == "" {
			return []Position{}, nil
		}
		id = row.providerID
	}
	if !providerIdentifier.MatchString(id) {
		return nil, ErrInvalidQuery
	}
	path := "/flights/" + id + "/track"
	query := url.Values{"include_estimated_positions": {"false"}}
	var response struct {
		Positions []aeroPosition `json:"positions"`
	}
	err := a.getJSON(ctx, path, query, &response)
	if errors.Is(err, ErrNotFound) {
		err = a.getJSON(ctx, "/history"+path, nil, &response)
	}
	if err != nil {
		return nil, err
	}
	if response.Positions == nil {
		return nil, fmt.Errorf("%w: %s response omitted positions", ErrUnavailable, aeroSource)
	}
	latest := a.now().Add(30 * time.Second)
	positions := make([]Position, 0, len(response.Positions))
	for _, row := range response.Positions {
		if position, ok := row.normalize(); ok && !position.At.After(latest) {
			positions = append(positions, position)
		}
	}
	sort.SliceStable(positions, func(i, j int) bool { return positions[i].At.Before(positions[j].At) })
	return slices.CompactFunc(positions, func(a, b Position) bool {
		return a.At.Equal(b.At) && a.Latitude == b.Latitude && a.Longitude == b.Longitude
	}), nil
}

func (a *AeroAPI) flightRows(ctx context.Context, path string, params url.Values) ([]aeroFlight, error) {
	var response struct {
		Flights []aeroFlight `json:"flights"`
	}
	if err := a.getJSON(ctx, path, params, &response); err != nil {
		return nil, err
	}
	if response.Flights == nil {
		return nil, fmt.Errorf("%w: %s response omitted flights", ErrUnavailable, aeroSource)
	}
	return response.Flights, nil
}

func (a *AeroAPI) getJSON(ctx context.Context, path string, params url.Values, target any) error {
	if a.key == "" {
		return fmt.Errorf("%w: FlightAware AeroAPI key is missing", ErrUnavailable)
	}
	endpoint := a.base + path
	if query := params.Encode(); query != "" {
		endpoint += "?" + query
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ErrInvalidQuery
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-apikey", a.key)
	body, err := responseBody(a.client, request, aeroSource)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("%w: %s returned invalid JSON", ErrUnavailable, aeroSource)
	}
	return nil
}

type aeroAirport struct {
	Code     string `json:"code"`
	ICAO     string `json:"code_icao"`
	IATA     string `json:"code_iata"`
	Timezone string `json:"timezone"`
}

type aeroFlight struct {
	Ident               string      `json:"ident"`
	IdentIATA           string      `json:"ident_iata"`
	IdentICAO           string      `json:"ident_icao"`
	ATCIdent            string      `json:"atc_ident"`
	ID                  string      `json:"fa_flight_id"`
	InboundID           string      `json:"inbound_fa_flight_id"`
	Status              string      `json:"status"`
	Origin              aeroAirport `json:"origin"`
	Destination         aeroAirport `json:"destination"`
	Blocked             bool        `json:"blocked"`
	Diverted            bool        `json:"diverted"`
	Cancelled           bool        `json:"cancelled"`
	Registration        string      `json:"registration"`
	AircraftType        string      `json:"aircraft_type"`
	RouteDistance       *int        `json:"route_distance"`
	ScheduledOut        *time.Time  `json:"scheduled_out"`
	EstimatedOut        *time.Time  `json:"estimated_out"`
	ActualOut           *time.Time  `json:"actual_out"`
	ScheduledOff        *time.Time  `json:"scheduled_off"`
	ActualOff           *time.Time  `json:"actual_off"`
	ScheduledOn         *time.Time  `json:"scheduled_on"`
	ActualOn            *time.Time  `json:"actual_on"`
	ScheduledIn         *time.Time  `json:"scheduled_in"`
	EstimatedIn         *time.Time  `json:"estimated_in"`
	ActualIn            *time.Time  `json:"actual_in"`
	GateOrigin          string      `json:"gate_origin"`
	GateDestination     string      `json:"gate_destination"`
	TerminalOrigin      string      `json:"terminal_origin"`
	TerminalDestination string      `json:"terminal_destination"`
	Baggage             string      `json:"baggage_claim"`
}

func (f aeroFlight) normalize(observedAt time.Time) Flight {
	departure := f.ScheduledOut
	if departure == nil {
		departure = f.ScheduledOff
	}
	arrival := f.ScheduledIn
	if arrival == nil {
		arrival = f.ScheduledOn
	}
	return Flight{
		ID: f.ID, providerID: f.ID, Number: first(f.IdentIATA, f.Ident),
		ICAONumber: f.IdentICAO, ATCIdent: f.ATCIdent,
		Origin:      first(f.Origin.IATA, f.Origin.ICAO, f.Origin.Code),
		Destination: first(f.Destination.IATA, f.Destination.ICAO, f.Destination.Code),
		OriginICAO:  f.Origin.ICAO, DestinationICAO: f.Destination.ICAO,
		OriginTimeZone: f.Origin.Timezone, DestinationTimeZone: f.Destination.Timezone,
		Status: f.normalizedStatus(), ProviderStatus: strings.TrimSpace(f.Status),
		ScheduledDeparture: departure, ScheduledArrival: arrival,
		EstimatedDeparture: f.EstimatedOut, EstimatedArrival: f.EstimatedIn,
		ActualDeparture: f.ActualOut, ActualTakeoff: f.ActualOff, ActualLanding: f.ActualOn, ActualArrival: f.ActualIn,
		DepartureGate: f.GateOrigin, ArrivalGate: f.GateDestination,
		DepartureTerminal: f.TerminalOrigin, ArrivalTerminal: f.TerminalDestination,
		TailNumber: f.Registration, AircraftType: f.AircraftType, DistanceMiles: f.RouteDistance,
		Baggage: f.Baggage, InboundID: f.InboundID, ObservedAt: observedAt, Source: aeroSource,
	}
}

func (a *AeroAPI) normalizeFlight(row aeroFlight, observedAt time.Time) Flight {
	flight := row.normalize(observedAt)
	if a.registry == nil || flight.TailNumber == "" || row.Cancelled || row.Diverted {
		return flight
	}
	switch flight.Status {
	case "scheduled", "boarding", "taxi_out", "airborne", "landed":
	default:
		return flight
	}
	// The current registry cannot identify the airframe of a historical
	// flight. Require a live assignment within the operating time window.
	departure := firstTime(flight.ActualDeparture, flight.ActualTakeoff, flight.ScheduledDeparture)
	if departure == nil || departure.Before(observedAt.Add(-36*time.Hour)) || departure.After(observedAt.Add(48*time.Hour)) {
		return flight
	}
	if flight.AircraftAge == nil {
		flight.AircraftAge = a.registry.Age(flight.TailNumber, departure.UTC())
	}
	if flight.AircraftModel == "" {
		flight.AircraftModel = a.registry.Model(flight.TailNumber)
	}
	return flight
}

func (f aeroFlight) normalizedStatus() string {
	label := strings.ToLower(strings.TrimSpace(f.Status))
	switch {
	case f.Diverted || strings.HasPrefix(label, "diverted"):
		return "diverted"
	case strings.HasPrefix(label, "cancelled") || strings.HasPrefix(label, "canceled"):
		return "cancelled"
	case f.ActualIn != nil || strings.HasPrefix(label, "arrived"):
		return "arrived"
	case f.ActualOn != nil || strings.HasPrefix(label, "landed"):
		return "landed"
	case f.ActualOff != nil || strings.HasPrefix(label, "en route") || strings.HasPrefix(label, "airborne"):
		return "airborne"
	case f.ActualOut != nil || strings.HasPrefix(label, "taxi out"):
		return "taxi_out"
	case strings.HasPrefix(label, "boarding"):
		return "boarding"
	case !f.Cancelled && (f.ScheduledOut != nil || f.ScheduledOff != nil || label == "scheduled"):
		return "scheduled"
	default:
		return "unknown"
	}
}

func first(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

type aeroPosition struct {
	Latitude       *float64  `json:"latitude"`
	Longitude      *float64  `json:"longitude"`
	Altitude       *float64  `json:"altitude"`
	AltitudeChange string    `json:"altitude_change"`
	GroundSpeed    *float64  `json:"groundspeed"`
	Heading        *float64  `json:"heading"`
	Timestamp      time.Time `json:"timestamp"`
	UpdateType     string    `json:"update_type"`
}

func (p aeroPosition) normalize() (Position, bool) {
	if p.Timestamp.IsZero() || p.Latitude == nil || p.Longitude == nil ||
		*p.Latitude < -90 || *p.Latitude > 90 || *p.Longitude < -180 || *p.Longitude > 180 || p.UpdateType == "P" {
		return Position{}, false
	}
	var altitudeFeet *float64
	if p.Altitude != nil {
		altitudeFeet = new(*p.Altitude * 100)
	}
	var verticalTrend string
	switch p.AltitudeChange {
	case "C":
		verticalTrend = "climbing"
	case "D":
		verticalTrend = "descending"
	case "-":
		verticalTrend = "level"
	}
	kind := map[string]string{
		"A": "ads_b", "O": "oceanic", "Z": "radar", "M": "multilateration",
		"D": "datalink", "X": "surface", "S": "space_based",
	}[p.UpdateType]
	return Position{
		Latitude: *p.Latitude, Longitude: *p.Longitude, AltitudeFeet: altitudeFeet,
		GroundSpeedKnots: p.GroundSpeed, VerticalTrend: verticalTrend, Heading: p.Heading, At: p.Timestamp.UTC(),
		Source: aeroSource, Kind: kind,
	}, true
}

// AeroProviderID returns a verified AeroAPI instance key when this flight has
// resolved to one. A schedule-only placeholder has no callback identity.
func (f Flight) AeroProviderID() string {
	if f.Source != aeroSource {
		return ""
	}
	if providerIdentifier.MatchString(f.providerID) {
		return f.providerID
	}
	if !strings.HasPrefix(f.ID, schedulePrefix) && providerIdentifier.MatchString(f.ID) {
		return f.ID
	}
	return ""
}
