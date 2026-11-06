package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

const (
	defaultConnectionBoardingReserveMinutes  = 15
	defaultConnectionSameTerminalMinutes     = 35
	defaultConnectionTerminalTransferMinutes = 65
	connectionMonitorGrace                   = 2 * time.Hour
	connectionAlertLifetime                  = 15 * time.Minute
)

func first(values ...*time.Time) *time.Time {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

type connectionResult struct {
	Risk                    string               `json:"risk"`
	AvailableMinutes        int                  `json:"availableMinutes"`
	TransferMinutes         int                  `json:"transferMinutes"`
	BoardingReserveMinutes  int                  `json:"boardingReserveMinutes"`
	ForecastTransferMinutes *int                 `json:"forecastTransferMinutes,omitempty"`
	Airport                 string               `json:"airport"`
	Instructions            []string             `json:"instructions"`
	MapURL                  string               `json:"mapUrl,omitempty"`
	GuideURL                string               `json:"guideUrl,omitempty"`
	Precision               string               `json:"precision"`
	ArrivalGate             string               `json:"arrivalGate"`
	DepartureGate           string               `json:"departureGate"`
	ArrivalGatePrediction   *core.GatePrediction `json:"arrivalGatePrediction,omitempty"`
	DepartureGatePrediction *core.GatePrediction `json:"departureGatePrediction,omitempty"`
	GuidanceAvailable       bool                 `json:"guidanceAvailable"`
	Monitoring              bool                 `json:"monitoring,omitempty"`
	MonitorEligible         bool                 `json:"monitorEligible,omitempty"`
}

func connectionIDs(r *http.Request) (string, string, error) {
	query := r.URL.Query()
	inID, outID := query.Get("inbound"), query.Get("outbound")
	if inID == outID || !flightID.MatchString(inID) || !flightID.MatchString(outID) {
		return "", "", errInvalid
	}
	return inID, outID, nil
}

func (s Server) connection(w http.ResponseWriter, r *http.Request, owner string) error {
	ctx := r.Context()
	inID, outID, err := connectionIDs(r)
	if err != nil {
		return err
	}
	in, out, err := s.Store.TrackedConnection(ctx, owner, inID, outID)
	if errors.Is(err, core.ErrNotFound) {
		return errInvalid
	}
	if err != nil {
		return err
	}
	now := time.Now()
	result, assessment, err := s.assessConnection(ctx, in, out, now, s.Store.PredictGate)
	if err != nil {
		return err
	}
	result.MonitorEligible = assessment.Monitorable && canStartConnection(in, out, now)
	result.Monitoring, err = s.Store.ConnectionMonitored(ctx, owner, inID, outID)
	if err != nil {
		return err
	}
	respond(w, 200, result)
	return nil
}

func (s Server) assessConnection(ctx context.Context, in, out feeds.Flight, now time.Time, predictGate core.ConnectionGatePredictor) (connectionResult, core.ConnectionAssessment, error) {
	var assessment core.ConnectionAssessment
	arrive := first(in.ActualArrival, in.EstimatedArrival, in.ScheduledArrival)
	depart := connectionWindowDeparture(out, now)
	if arrive == nil || depart == nil || !connectionAirportsMatch(in, out) {
		return connectionResult{}, assessment, errInvalid
	}
	route, configured := s.ConnectionRules.routeForConnection(in, out)
	result := estimateConnectionRoute(in, out, route, configured)
	result.ArrivalGate, result.DepartureGate = in.ArrivalGate, out.DepartureGate
	if !configured {
		result.BoardingReserveMinutes = defaultConnectionBoardingReserveMinutes
	}
	planningMinutes := result.TransferMinutes + result.BoardingReserveMinutes
	departed := connectionDeparted(out)
	disrupted := connectionDisrupted(in, out)
	result.GuidanceAvailable = !departed && !disrupted
	var err error
	result.ArrivalGatePrediction, err = predictGate(ctx, in, true, now)
	if err != nil {
		return connectionResult{}, assessment, err
	}
	result.DepartureGatePrediction, err = predictGate(ctx, out, false, now)
	if err != nil {
		return connectionResult{}, assessment, err
	}
	if result.GuidanceAvailable {
		// Configured routes already include the operator's boarding allowance.
		// An inferred gate may increase the total without replacing directions.
		result.ForecastTransferMinutes = s.ConnectionRules.forecastTransfer(in, out, route.Precision, result.ArrivalGatePrediction, result.DepartureGatePrediction)
		if result.ForecastTransferMinutes != nil {
			planningMinutes = max(planningMinutes, *result.ForecastTransferMinutes)
		}
	}
	result.AvailableMinutes, result.Risk = connectionRisk(in, out, *arrive, *depart, now, planningMinutes, departed)
	assessment.Risk = result.Risk
	staleAt := depart.Add(connectionMonitorGrace)
	assessment.Monitorable = !departed && !disrupted && now.Before(staleAt)
	if assessment.Monitorable {
		assessment.MonitorUntil = staleAt
		assessment.NextCheck = nextConnectionCheck(in, *depart, now, planningMinutes, result.Risk, staleAt)
		assessment.AlertUntil = earlierTime(now.Add(connectionAlertLifetime), staleAt)
	} else if departed || disrupted {
		assessment.AlertUntil = terminalConnectionAlertUntil(in, out, now)
	}
	return result, assessment, nil
}

func canStartConnection(in, out feeds.Flight, now time.Time) bool {
	depart := first(out.EstimatedDeparture, out.ScheduledDeparture)
	if in.ScheduledArrival == nil || out.ScheduledDeparture == nil || depart == nil || !depart.After(now) {
		return false
	}
	window := out.ScheduledDeparture.Sub(*in.ScheduledArrival)
	return window > 0 && window <= 24*time.Hour
}

func connectionDisrupted(in, out feeds.Flight) bool {
	return in.Status == "cancelled" || in.Status == "diverted" || out.Status == "cancelled"
}

func terminalConnectionAlertUntil(in, out feeds.Flight, now time.Time) time.Time {
	observed := out.ObservedAt
	if in.Status == "cancelled" || in.Status == "diverted" {
		observed = in.ObservedAt
	}
	expires := earlierTime(now, observed).Add(connectionAlertLifetime)
	if depart := connectionWindowDeparture(out, now); depart != nil {
		expires = earlierTime(expires, depart.Add(connectionMonitorGrace))
	}
	return expires
}

func connectionDeparture(flight feeds.Flight) *time.Time {
	return first(flight.ActualDeparture, flight.ActualTakeoff, flight.EstimatedDeparture, flight.ScheduledDeparture)
}

func connectionWindowDeparture(flight feeds.Flight, now time.Time) *time.Time {
	departure := connectionDeparture(flight)
	if !connectionDeparted(flight) || flight.ActualDeparture != nil && !flight.ActualDeparture.After(now) {
		return departure
	}
	// Without a gate-out time, confirmed movement bounds the estimate. A
	// stale schedule or ETD must not extend a window that has already closed.
	latest := now
	if !flight.ObservedAt.IsZero() {
		latest = earlierTime(latest, flight.ObservedAt)
	}
	for _, bound := range []*time.Time{departure, flight.ActualTakeoff, flight.ActualLanding, flight.ActualArrival} {
		if bound != nil {
			latest = earlierTime(latest, *bound)
		}
	}
	return &latest
}

func earlierTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func nextConnectionCheck(in feeds.Flight, departure, now time.Time, planningMinutes int, risk string, staleAt time.Time) time.Time {
	next := earlierTime(now.Add(5*time.Minute), staleAt)
	if departure.After(now) {
		next = earlierTime(next, departure)
	}
	if in.ActualArrival != nil {
		return next
	}
	switch risk {
	case "Relaxed":
		planningMinutes += 25
	case "Tight":
	default:
		return next
	}
	threshold := departure.Add(-time.Duration(planningMinutes)*time.Minute + time.Millisecond)
	if threshold.After(now) {
		next = earlierTime(next, threshold)
	}
	return next
}

func connectionAirportsMatch(in, out feeds.Flight) bool {
	if in.DestinationICAO != "" && out.OriginICAO != "" {
		return in.DestinationICAO == out.OriginICAO
	}
	return in.Destination != "" && in.Destination == out.Origin
}

func estimateConnectionRoute(in, out feeds.Flight, route ConnectionRoute, configured bool) connectionResult {
	airport := in.Destination
	if airport == "" {
		airport = in.DestinationICAO
	}
	transfer := 50
	instructions := append([]string{}, route.Instructions...)
	if len(instructions) == 0 {
		instructions = append(instructions, "Follow connections signage at "+airport+".")
	}
	instructions = append(instructions, "Check the gate and boarding deadline with the airline.")
	if in.ArrivalTerminal != "" && out.DepartureTerminal != "" {
		transfer = defaultConnectionSameTerminalMinutes
		if in.ArrivalTerminal != out.DepartureTerminal {
			transfer = defaultConnectionTerminalTransferMinutes
			if len(route.Instructions) == 0 {
				instructions = append(instructions, fmt.Sprintf("Transfer from terminal %s to terminal %s.", in.ArrivalTerminal, out.DepartureTerminal))
			}
		}
	}
	// Verified built-in routes distinguish terminal aliases from changes
	// that require another security checkpoint.
	if !configured && route.TransferMinutes > 0 {
		transfer = route.TransferMinutes
	}
	instructions = append(instructions, "Allow extra time if your itinerary requires security, passport control or baggage recheck.")
	if out.DepartureGate != "" {
		instructions = append(instructions, "Your reported departure gate is "+out.DepartureGate+".")
	}
	precision := "generic"
	if configured {
		transfer, precision = route.TransferMinutes, route.Precision
		instructions = append(append([]string{}, route.Instructions...), "Check the gate and boarding deadline with the airline.")
	}
	return connectionResult{TransferMinutes: transfer, Airport: airport, Instructions: instructions,
		MapURL: route.MapURL, GuideURL: route.GuideURL, Precision: precision}
}

func (rules *ConnectionRules) forecastTransfer(in, out feeds.Flight, precision string, arrival, departure *core.GatePrediction) *int {
	if arrival == nil && departure == nil {
		return nil
	}
	arrivalGate, departureGate := in.ArrivalGate, out.DepartureGate
	if arrivalGate == "" && arrival != nil {
		arrivalGate = arrival.Gate
	}
	if departureGate == "" && departure != nil {
		departureGate = departure.Gate
	}
	forecast, ok := rules.Route(in.Destination, in.DestinationICAO, in.ArrivalTerminal, arrivalGate, out.DepartureTerminal, departureGate)
	if !ok || forecast.Precision == precision || forecast.Precision == "terminals" {
		return nil
	}
	return &forecast.TransferMinutes
}

func connectionDeparted(out feeds.Flight) bool {
	if first(out.ActualDeparture, out.ActualTakeoff, out.ActualLanding, out.ActualArrival) != nil {
		return true
	}
	switch out.Status {
	case "taxi_out", "airborne", "landed", "arrived", "diverted":
		return true
	default:
		return false
	}
}

func connectionRisk(in, out feeds.Flight, arrival, departure, now time.Time, planningMinutes int, departed bool) (int, string) {
	if !departed && in.ActualArrival == nil && arrival.Before(now) {
		arrival = now
	}
	window := departure.Sub(arrival)
	minutes := int(window / time.Minute)
	required := time.Duration(planningMinutes) * time.Minute
	switch {
	case connectionDisrupted(in, out) || window <= 0:
		return minutes, "Missed"
	case departed && (in.ActualArrival == nil || window < required):
		return minutes, "Missed"
	case window < required || !departed && !departure.After(now):
		return minutes, "Risky"
	case window < required+25*time.Minute:
		return minutes, "Tight"
	default:
		return minutes, "Relaxed"
	}
}

// PublicOrigin rejects URL components that would put access tokens on another host.
func PublicOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.Path != "" && u.Path != "/" || u.Scheme != "http" && u.Scheme != "https" {
		return "", errInvalid
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}
