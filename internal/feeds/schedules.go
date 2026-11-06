package feeds

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const schedulePrefix = "scheduled_"

var scheduledDesignator = regexp.MustCompile(`^([A-Z]{3}|[A-Z][A-Z0-9]|[0-9][A-Z])([0-9]{1,5})$\z`)

type aeroSchedule struct {
	ID                 string     `json:"fa_flight_id"`
	Ident              string     `json:"ident"`
	IdentICAO          string     `json:"ident_icao"`
	IdentIATA          string     `json:"ident_iata"`
	OperatingIdent     string     `json:"actual_ident"`
	OperatingIdentICAO string     `json:"actual_ident_icao"`
	Origin             string     `json:"origin"`
	OriginICAO         string     `json:"origin_icao"`
	OriginIATA         string     `json:"origin_iata"`
	Destination        string     `json:"destination"`
	DestinationICAO    string     `json:"destination_icao"`
	DestinationIATA    string     `json:"destination_iata"`
	AircraftType       string     `json:"aircraft_type"`
	Departure          *time.Time `json:"scheduled_out"`
	Arrival            *time.Time `json:"scheduled_in"`
}

type scheduleKey struct {
	Number          string
	OperatingNumber string
	Origin          string
	Destination     string
	Departure       time.Time
}

func (s aeroSchedule) key() scheduleKey {
	return scheduleKey{
		Number:          first(s.IdentIATA, s.IdentICAO, s.Ident),
		OperatingNumber: first(s.OperatingIdentICAO, s.OperatingIdent, s.IdentICAO, s.Ident),
		Origin:          first(s.OriginICAO, s.OriginIATA, s.Origin),
		Destination:     first(s.DestinationICAO, s.DestinationIATA, s.Destination),
		Departure:       s.Departure.UTC(),
	}
}

func (k scheduleKey) id() string {
	// All values are validated before constructing the key, so it is safe to
	// encode them together and use the result as one URL segment.
	raw := strings.Join([]string{k.Number, k.OperatingNumber, k.Origin, k.Destination, k.Departure.Format(time.RFC3339)}, "|")
	return schedulePrefix + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func (k scheduleKey) valid() bool {
	return designator.MatchString(k.Number) && designator.MatchString(k.OperatingNumber) &&
		providerIdentifier.MatchString(k.Origin) && providerIdentifier.MatchString(k.Destination) &&
		!k.Departure.IsZero()
}

func parseScheduleKey(id string) (scheduleKey, error) {
	if len(id) > 128 {
		return scheduleKey{}, ErrInvalidQuery
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, schedulePrefix))
	if err != nil {
		return scheduleKey{}, ErrInvalidQuery
	}
	fields := strings.Split(string(decoded), "|")
	if len(fields) != 5 {
		return scheduleKey{}, ErrInvalidQuery
	}
	departure, err := time.Parse(time.RFC3339, fields[4])
	if err != nil {
		return scheduleKey{}, ErrInvalidQuery
	}
	key := scheduleKey{fields[0], fields[1], fields[2], fields[3], departure}
	if !key.valid() || key.id() != id {
		return scheduleKey{}, ErrInvalidQuery
	}
	return key, nil
}

func (a *AeroAPI) scheduledRows(ctx context.Context, number string, start, end time.Time) ([]aeroSchedule, error) {
	parts := scheduledDesignator.FindStringSubmatch(number)
	if parts == nil {
		return []aeroSchedule{}, nil
	}
	query := url.Values{"airline": {parts[1]}, "flight_number": {parts[2]}, "max_pages": {"3"}}
	path := "/schedules/" + start.Format(time.RFC3339) + "/" + end.Format(time.RFC3339)
	var response struct {
		Rows []aeroSchedule `json:"scheduled"`
	}
	if err := a.getJSON(ctx, path, query, &response); err != nil {
		return nil, err
	}
	if response.Rows == nil {
		return nil, fmt.Errorf("%w: %s response omitted schedules", ErrUnavailable, aeroSource)
	}
	return response.Rows, nil
}

func (a *AeroAPI) future(ctx context.Context, number string, date, start, end time.Time) ([]Flight, error) {
	if !start.Before(a.now().UTC().AddDate(1, 0, 0)) {
		return []Flight{}, nil
	}
	if limit := a.now().UTC().AddDate(1, 0, 0); end.After(limit) {
		end = limit
	}
	rows, err := a.scheduledRows(ctx, number, start, end)
	if errors.Is(err, ErrNotFound) {
		return []Flight{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]Flight, 0, len(rows))
	seen := make(map[string]bool)
	timezones := make(map[string]string)
	for _, row := range rows {
		if row.Departure == nil {
			continue
		}
		key := row.key()
		if !key.valid() || seen[key.id()] {
			continue
		}
		zone, known := timezones[key.Origin]
		if !known {
			zone, err = a.airportTimezone(ctx, key.Origin)
			if err != nil {
				return nil, err
			}
			timezones[key.Origin] = zone
		}
		if !onDepartureDate(*row.Departure, zone, date) {
			continue
		}
		seen[key.id()] = true
		flight := row.normalize(key, a.now().UTC())
		flight.OriginTimeZone = zone
		result = append(result, flight)
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].ScheduledDeparture.Before(*result[j].ScheduledDeparture)
	})
	return result, nil
}

func (a *AeroAPI) airportTimezone(ctx context.Context, code string) (string, error) {
	airport, err := a.Airport(ctx, code)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return airport.TimeZone, err
}

func (s aeroSchedule) normalize(key scheduleKey, now time.Time) Flight {
	flight := Flight{
		ID: key.id(), Number: key.Number, Origin: first(s.OriginIATA, key.Origin), Destination: first(s.DestinationIATA, key.Destination),
		OriginICAO: s.OriginICAO, DestinationICAO: s.DestinationICAO,
		Status: "scheduled", ScheduledDeparture: s.Departure, ScheduledArrival: s.Arrival,
		AircraftType: s.AircraftType, ObservedAt: now, Source: "flightaware_schedule",
	}
	if aeroAlertIdent.MatchString(key.OperatingNumber) {
		flight.ICAONumber = key.OperatingNumber
	}
	if providerIdentifier.MatchString(s.ID) {
		flight.ID, flight.providerID = s.ID, s.ID
	}
	return flight
}

func (a *AeroAPI) getScheduled(ctx context.Context, id string) (Flight, error) {
	key, err := parseScheduleKey(id)
	if err != nil {
		return Flight{}, err
	}
	now := a.now().UTC()
	if key.Departure.Before(now.Add(45 * time.Hour)) {
		if flight, found, err := a.resolveSchedule(ctx, key, now); err != nil {
			return Flight{}, err
		} else if found {
			return flight, nil
		}
	}
	if key.Departure.Before(now.Add(-3 * time.Hour)) {
		// An expired timetable entry must not masquerade as a live flight.
		return Flight{}, ErrNotFound
	}
	rows, err := a.scheduledRows(ctx, key.Number, key.Departure.Add(-3*time.Hour), key.Departure.Add(3*time.Hour))
	if err != nil {
		return Flight{}, err
	}
	for _, row := range rows {
		if row.Departure == nil || row.key().id() != id {
			continue
		}
		flight := row.normalize(key, now)
		flight.ID = id
		return flight, nil
	}
	return Flight{}, ErrNotFound
}

func (a *AeroAPI) resolveSchedule(ctx context.Context, key scheduleKey, now time.Time) (Flight, bool, error) {
	start := key.Departure.Add(-3 * time.Hour)
	end := key.Departure.Add(3 * time.Hour)
	params := url.Values{"ident_type": {"designator"}, "start": {start.Format(time.RFC3339)}, "end": {end.Format(time.RFC3339)}}
	path := "/flights/"
	if start.Before(now.Add(-10 * 24 * time.Hour)) {
		path = "/history/flights/"
	}
	rows, err := a.flightRows(ctx, path+key.OperatingNumber, params)
	if errors.Is(err, ErrNotFound) {
		return Flight{}, false, nil
	}
	if err != nil {
		return Flight{}, false, err
	}
	var best Flight
	var bestDistance time.Duration
	tie := false
	for _, row := range rows {
		if row.Blocked || !providerIdentifier.MatchString(row.ID) ||
			!matchesAirport(row.Origin, key.Origin) || !matchesAirport(row.Destination, key.Destination) {
			continue
		}
		flight := row.normalize(now)
		if flight.ScheduledDeparture == nil {
			continue
		}
		distance := flight.ScheduledDeparture.Sub(key.Departure).Abs()
		if distance > 3*time.Hour {
			continue
		}
		switch {
		case best.ID == "" || distance < bestDistance:
			best, bestDistance, tie = flight, distance, false
		case distance == bestDistance && best.ID != flight.ID:
			tie = true
		}
	}
	if best.ID == "" || tie {
		return Flight{}, false, nil
	}
	best.ID = key.id()
	return best, true, nil
}

func matchesAirport(airport aeroAirport, code string) bool {
	return code == airport.ICAO || code == airport.IATA || code == airport.Code
}
