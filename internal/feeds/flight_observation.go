package feeds

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxFlightObservationBytes = 64 << 10

// DecodeFlightObservation reads an operator bridge's complete AeroAPI flight
// snapshot. The caller verifies the signature before trusting its capture time.
func DecodeFlightObservation(input io.Reader, now time.Time) (Flight, error) {
	body, err := io.ReadAll(io.LimitReader(input, MaxFlightObservationBytes+1))
	if err != nil || len(body) > MaxFlightObservationBytes || !utf8.Valid(body) {
		return Flight{}, fmt.Errorf("%w: invalid flight observation body", ErrInvalidQuery)
	}
	var envelope struct {
		Version    int             `json:"version"`
		ObservedAt time.Time       `json:"observedAt"`
		Flight     json.RawMessage `json:"flight"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Version != 1 ||
		envelope.ObservedAt.IsZero() || envelope.ObservedAt.Before(now.Add(-5*time.Minute)) || envelope.ObservedAt.After(now) {
		return Flight{}, fmt.Errorf("%w: invalid flight observation envelope", ErrInvalidQuery)
	}

	var fields map[string]json.RawMessage
	var row aeroFlight
	if json.Unmarshal(envelope.Flight, &fields) != nil || json.Unmarshal(envelope.Flight, &row) != nil {
		return Flight{}, fmt.Errorf("%w: invalid AeroAPI snapshot", ErrInvalidQuery)
	}
	if row.Blocked || string(fields["blocked"]) == "true" {
		return Flight{}, ErrNotFound
	}
	if value := fields["error"]; len(value) != 0 && string(value) != "null" && string(value) != `""` {
		return Flight{}, ErrNotFound
	}
	// A missing mutable field in a partial message cannot clear the value
	// already stored for the flight. BaseFlight supplies these keys even when
	// their values are unknown and therefore null.
	for _, field := range []string{
		"registration", "aircraft_type", "route_distance", "inbound_fa_flight_id", "atc_ident",
		"gate_origin", "gate_destination", "terminal_origin", "terminal_destination", "baggage_claim",
		"scheduled_out", "scheduled_off", "scheduled_in", "scheduled_on", "estimated_out", "estimated_in",
		"actual_out", "actual_off", "actual_on", "actual_in",
	} {
		if _, exists := fields[field]; !exists {
			return Flight{}, fmt.Errorf("%w: incomplete AeroAPI snapshot", ErrInvalidQuery)
		}
	}
	for _, field := range []string{"blocked", "cancelled", "diverted"} {
		if value := string(fields[field]); value != "false" && value != "true" {
			return Flight{}, fmt.Errorf("%w: incomplete AeroAPI snapshot", ErrInvalidQuery)
		}
	}

	flight := row.normalize(envelope.ObservedAt.UTC())
	if !providerIdentifier.MatchString(flight.ID) || !designator.MatchString(flight.Number) ||
		flight.ICAONumber != "" && !designator.MatchString(flight.ICAONumber) ||
		flight.InboundID != "" && (!providerIdentifier.MatchString(flight.InboundID) || flight.InboundID == flight.ID) ||
		!airportCode.MatchString(flight.Origin) || !airportCode.MatchString(flight.Destination) ||
		flight.OriginICAO != "" && !airportCode.MatchString(flight.OriginICAO) ||
		flight.DestinationICAO != "" && !airportCode.MatchString(flight.DestinationICAO) ||
		flight.ProviderStatus == "" || flight.DistanceMiles != nil && (*flight.DistanceMiles < 0 || *flight.DistanceMiles > 40_000) ||
		!validFlightObservationTimes(flight, now) || !validFlightObservationLabels(flight) {
		return Flight{}, fmt.Errorf("%w: invalid AeroAPI snapshot fields", ErrInvalidQuery)
	}
	return flight, nil
}

func validFlightObservationTimes(flight Flight, now time.Time) bool {
	if flight.ScheduledDeparture == nil || flight.ScheduledDeparture.Before(now.Add(-36*time.Hour)) || flight.ScheduledDeparture.After(now.Add(48*time.Hour)) {
		return false
	}
	for _, at := range []*time.Time{flight.ScheduledDeparture, flight.ScheduledArrival, flight.EstimatedDeparture, flight.EstimatedArrival} {
		if at != nil && (at.IsZero() || at.Before(now.AddDate(0, 0, -7)) || at.After(now.AddDate(0, 0, 7))) {
			return false
		}
	}
	var previous time.Time
	for _, at := range []*time.Time{flight.ActualDeparture, flight.ActualTakeoff, flight.ActualLanding, flight.ActualArrival} {
		if at == nil {
			continue
		}
		if at.IsZero() || at.Before(now.AddDate(0, 0, -7)) || at.After(flight.ObservedAt) || at.Before(previous) {
			return false
		}
		previous = *at
	}
	return true
}

func validFlightObservationLabels(flight Flight) bool {
	for _, label := range []string{flight.ATCIdent, flight.OriginTimeZone, flight.DestinationTimeZone, flight.ProviderStatus,
		flight.DepartureGate, flight.ArrivalGate, flight.DepartureTerminal, flight.ArrivalTerminal,
		flight.TailNumber, flight.AircraftType, flight.Baggage} {
		if len(label) > 128 || strings.ContainsFunc(label, func(r rune) bool {
			return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
		}) {
			return false
		}
	}
	return true
}
