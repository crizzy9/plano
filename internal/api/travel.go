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
	Airport                 string               `json:"airport"`
	Instructions            []string             `json:"instructions"`
	MapURL                  string               `json:"mapUrl,omitempty"`
	Precision               string               `json:"precision"`
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
	arrive, depart := first(in.ActualArrival, in.EstimatedArrival, in.ScheduledArrival), first(out.ActualDeparture, out.EstimatedDeparture, out.ScheduledDeparture)
	if arrive == nil || depart == nil || in.Destination == "" || in.Destination != out.Origin {
		return errInvalid
	}
	route, configured := s.ConnectionRules.Route(in.Destination, in.DestinationICAO, in.ArrivalTerminal, in.ArrivalGate, out.DepartureTerminal, out.DepartureGate)
	result := estimateConnection(in, out, *arrive, *depart, route, configured)
	result.ArrivalGatePrediction, err = s.Store.PredictGate(r.Context(), in, true, time.Now())
	if err != nil {
		return err
	}
	result.DepartureGatePrediction, err = s.Store.PredictGate(r.Context(), out, false, time.Now())
	if err != nil {
		return err
	}
	respond(w, 200, result)
	return nil
}

func estimateConnection(in, out feeds.Flight, arrival, departure time.Time, route ConnectionRoute, configured bool) connectionResult {
	minutes := int(departure.Sub(arrival).Minutes())
	transfer := 50
	instructions := []string{"Follow connections signage at " + in.Destination + ".", "Check the gate and boarding deadline with the airline."}
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
	risk := "Relaxed"
	switch {
	case out.Status == "cancelled" || in.Status == "cancelled" || in.Status == "diverted" || minutes <= 0:
		risk = "Missed"
	case minutes < transfer:
		risk = "Risky"
	case minutes < transfer+25:
		risk = "Tight"
	}
	return connectionResult{Risk: risk, AvailableMinutes: minutes, TransferMinutes: transfer, Airport: in.Destination, Instructions: instructions, MapURL: route.MapURL, Precision: precision}
}

// PublicOrigin rejects URL components that would put access tokens on another host.
func PublicOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || u.Scheme != "http" && u.Scheme != "https" {
		return "", errInvalid
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}
