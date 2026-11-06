package core

import (
	"slices"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

var flightStateAlertKinds = [...]string{
	"boarding", "check_in", "departure_gate", "etd", "delay_prediction",
	"arrival_gate", "eta", "baggage", "tail", "cancelled", "diverted",
}

func flightAlertSupersedable(kind string) bool {
	return slices.Contains(flightStateAlertKinds[:], kind)
}

func checkInDeadline(f feeds.Flight, now time.Time) *time.Time {
	departure := effectiveDeparture(f)
	if departure == nil || !flightAlertRelevant("check_in", f) || now.Before(departure.Add(-24*time.Hour)) || !now.Before(*departure) {
		return nil
	}
	return departure
}

func flightAlertRelevant(kind string, f feeds.Flight) bool {
	// A later OOOI timestamp also rules out a predeparture alert when an
	// incomplete observation omits an earlier timestamp or phase.
	beforeDeparture := f.ActualLanding == nil && f.ActualArrival == nil && flightBeforeDeparture(f)
	arrivalAirportRelevant := f.Status != "cancelled" && f.Status != "diverted"
	switch kind {
	case "boarding":
		return f.Status == "boarding" && beforeDeparture
	case "check_in":
		return effectiveDeparture(f) != nil && beforeDeparture
	case "departure_gate":
		return strings.TrimSpace(f.DepartureGate) != "" && beforeDeparture
	case "etd":
		return f.EstimatedDeparture != nil && beforeDeparture
	case "delay_prediction":
		return beforeDeparture
	case "arrival_gate":
		return arrivalAirportRelevant && strings.TrimSpace(f.ArrivalGate) != ""
	case "eta":
		return arrivalAirportRelevant && f.Status != "arrived" && f.ActualArrival == nil && f.EstimatedArrival != nil
	case "baggage":
		return arrivalAirportRelevant && strings.TrimSpace(f.Baggage) != ""
	case "tail":
		return f.Status != "cancelled" && strings.TrimSpace(f.TailNumber) != ""
	case "cancelled", "diverted":
		return f.Status == kind
	default:
		return true
	}
}

func supersededFlightAlertKinds(before, after feeds.Flight, events []Event) []string {
	originChanged := flightAlertAirportChanged(before.Origin, before.OriginICAO, after.Origin, after.OriginICAO)
	destinationChanged := flightAlertAirportChanged(before.Destination, before.DestinationICAO, after.Destination, after.DestinationICAO)
	var obsolete []string
	for _, kind := range flightStateAlertKinds {
		becameIrrelevant := flightAlertRelevant(kind, before) && !flightAlertRelevant(kind, after)
		if becameIrrelevant ||
			flightAlertInputsChanged(kind, before, after, originChanged, destinationChanged) ||
			slices.ContainsFunc(events, func(event Event) bool { return event.Kind == kind }) {
			obsolete = append(obsolete, kind)
		}
	}
	return obsolete
}

func flightAlertInputsChanged(kind string, before, after feeds.Flight, originChanged, destinationChanged bool) bool {
	switch kind {
	case "boarding", "etd":
		return originChanged
	case "check_in":
		return checkInDeadline(before, after.ObservedAt) != nil && checkInDeadline(after, after.ObservedAt) == nil
	case "delay_prediction":
		return originChanged || before.TailNumber != after.TailNumber || before.InboundID != after.InboundID
	case "departure_gate":
		return originChanged || before.DepartureGate != after.DepartureGate
	case "eta":
		return destinationChanged
	case "arrival_gate":
		return destinationChanged || before.ArrivalGate != after.ArrivalGate
	case "baggage":
		return destinationChanged || before.Baggage != after.Baggage
	case "tail":
		return before.TailNumber != after.TailNumber
	default:
		return false
	}
}

func flightAlertAirportChanged(before, beforeICAO, after, afterICAO string) bool {
	if beforeICAO != "" && afterICAO != "" {
		return !strings.EqualFold(beforeICAO, afterICAO)
	}
	// The same airport can appear as an IATA code, an ICAO code, or both as
	// observations gain or lose fields. Use only aliases present in the data.
	for _, code := range []string{before, beforeICAO} {
		if code != "" && (strings.EqualFold(code, after) || strings.EqualFold(code, afterICAO)) {
			return false
		}
	}
	return before != "" || beforeICAO != ""
}
