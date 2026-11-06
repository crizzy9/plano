package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

const defaultConnectionBoardingReserveMinutes = 15

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
	Precision               string               `json:"precision"`
	ArrivalGate             string               `json:"arrivalGate"`
	DepartureGate           string               `json:"departureGate"`
	ArrivalGatePrediction   *core.GatePrediction `json:"arrivalGatePrediction,omitempty"`
	DepartureGatePrediction *core.GatePrediction `json:"departureGatePrediction,omitempty"`
}

func (s Server) connection(w http.ResponseWriter, r *http.Request, owner string) error {
	query := r.URL.Query()
	inID, outID := query.Get("inbound"), query.Get("outbound")
	if inID == outID || !flightID.MatchString(inID) || !flightID.MatchString(outID) {
		return errInvalid
	}
	flights, err := s.Store.Tracked(r.Context(), owner)
	if err != nil {
		return err
	}
	var in, out feeds.Flight
	for _, f := range flights {
		if f.ID == inID {
			in = f
		}
		if f.ID == outID {
			out = f
		}
	}
	arrive := first(in.ActualArrival, in.EstimatedArrival, in.ScheduledArrival)
	depart := first(out.ActualDeparture, out.EstimatedDeparture, out.ScheduledDeparture, out.ActualTakeoff)
	if arrive == nil || depart == nil || !connectionAirportsMatch(in, out) {
		return errInvalid
	}
	route, configured := s.ConnectionRules.Route(in.Destination, in.DestinationICAO, in.ArrivalTerminal, in.ArrivalGate, out.DepartureTerminal, out.DepartureGate)
	result := estimateConnectionRoute(in, out, route, configured)
	result.ArrivalGate, result.DepartureGate = in.ArrivalGate, out.DepartureGate
	now := time.Now()
	result.ArrivalGatePrediction, err = s.Store.PredictGate(r.Context(), in, true, now)
	if err != nil {
		return err
	}
	result.DepartureGatePrediction, err = s.Store.PredictGate(r.Context(), out, false, now)
	if err != nil {
		return err
	}
	if !configured {
		result.BoardingReserveMinutes = defaultConnectionBoardingReserveMinutes
	}
	planningMinutes := result.TransferMinutes + result.BoardingReserveMinutes
	departed := connectionDeparted(out)
	if !departed && out.Status != "cancelled" && in.Status != "cancelled" && in.Status != "diverted" {
		// Configured routes already include the operator's boarding allowance.
		// An inferred gate may increase the total without replacing directions.
		result.ForecastTransferMinutes = s.ConnectionRules.forecastTransfer(in, out, route.Precision, result.ArrivalGatePrediction, result.DepartureGatePrediction)
		if result.ForecastTransferMinutes != nil {
			planningMinutes = max(planningMinutes, *result.ForecastTransferMinutes)
		}
	}
	result.AvailableMinutes, result.Risk = connectionRisk(in, out, *arrive, *depart, now, planningMinutes, departed)
	respond(w, 200, result)
	return nil
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
	instructions := []string{"Follow connections signage at " + airport + ".", "Check the gate and boarding deadline with the airline."}
	if in.ArrivalTerminal != "" && out.DepartureTerminal != "" {
		transfer = 35
		if in.ArrivalTerminal != out.DepartureTerminal {
			transfer = 65
			instructions = append(instructions, fmt.Sprintf("Transfer from terminal %s to terminal %s.", in.ArrivalTerminal, out.DepartureTerminal))
		}
	}
	instructions = append(instructions, "Allow extra time if your itinerary requires security, passport control or baggage recheck.")
	if out.DepartureGate != "" {
		instructions = append(instructions, "Continue to gate "+out.DepartureGate+".")
	}
	precision := "generic"
	if configured {
		transfer, precision = route.TransferMinutes, route.Precision
		instructions = append(append([]string{}, route.Instructions...), "Check the gate and boarding deadline with the airline.")
	}
	return connectionResult{TransferMinutes: transfer, Airport: airport, Instructions: instructions, MapURL: route.MapURL, Precision: precision}
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
	case out.Status == "cancelled" || in.Status == "cancelled" || in.Status == "diverted" || window <= 0:
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
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || u.Scheme != "http" && u.Scheme != "https" {
		return "", errInvalid
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}
