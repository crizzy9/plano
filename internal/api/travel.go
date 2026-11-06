package api

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) share(w http.ResponseWriter, r *http.Request, owner string) error {
	var input struct {
		FlightID string `json:"flightId"`
	}
	if err := parse(w, r, &input, 4096); err != nil {
		return err
	}
	if !flightID.MatchString(input.FlightID) {
		return errInvalid
	}
	expires := time.Now().UTC().Add(72 * time.Hour)
	token, err := s.Store.Share(r.Context(), owner, input.FlightID, expires)
	if err != nil {
		return err
	}
	respond(w, 201, map[string]any{"url": s.PublicURL + "/share/" + token, "expiresAt": expires})
	return nil
}

var shareTemplate = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex,nofollow"><meta http-equiv="refresh" content="45"><title>Plano • {{.Flight.Number}}</title></head><body><main>
<h1>{{.Flight.Number}} · {{.Flight.Origin}} to {{.Flight.Destination}}</h1>
<p>Status {{.Status}}</p>{{if .Arrival}}<p>Arrival {{.Arrival}}</p>{{end}}
{{with .Flight.ArrivalTerminal}}<p>Terminal {{.}}</p>{{end}}{{with .Flight.ArrivalGate}}<p>Gate {{.}}</p>{{end}}{{with .Flight.Baggage}}<p>Baggage carousel {{.}}</p>{{end}}
<p>Observed {{.Flight.ObservedAt.UTC.Format "15:04 UTC on 02 Jan 2006"}}</p>{{if .Demo}}<p>Synthetic demo flight</p>{{end}}</main></body></html>`))

func (s Server) shared(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if len(token) < 20 || len(token) > 128 {
		http.NotFound(w, r)
		return
	}
	flight, err := s.Store.Shared(r.Context(), token, time.Now())
	if err != nil {
		s.failure(w, err)
		return
	}
	var arrival string
	if instant := first(flight.ActualArrival, flight.EstimatedArrival, flight.ScheduledArrival); instant != nil {
		location, err := time.LoadLocation(flight.DestinationTimeZone)
		if err == nil && flight.DestinationTimeZone != "" {
			instantLocal := instant.In(location)
			instant = &instantLocal
		}
		arrival = instant.Format("15:04 MST on 02 Jan 2006")
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := shareTemplate.Execute(w, struct {
		Flight          feeds.Flight
		Status, Arrival string
		Demo            bool
	}{flight, strings.ReplaceAll(flight.Status, "_", " "), arrival, s.Demo}); err != nil {
		s.Logger.Warn("shared flight rendering stopped")
	}
}

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

type airportPoint struct{ lat, lon float64 }

func (s Server) passport(w http.ResponseWriter, r *http.Request, owner string) error {
	tracked, err := s.Store.Tracked(r.Context(), owner)
	if err != nil {
		return err
	}
	flights := make([]feeds.Flight, 0)
	segments := make([]passportSegment, 0)
	stats := passportStats{}
	aircraft := make(map[string]bool)
	points := make(map[string]*airportPoint)
	for _, f := range tracked {
		if f.Status != "arrived" {
			continue
		}
		flights = append(flights, f)
		stats.Flights++
		if f.TailNumber != "" {
			aircraft[f.TailNumber] = true
		}
		if f.ActualDeparture != nil && f.ActualArrival != nil && f.ActualArrival.After(*f.ActualDeparture) {
			stats.Hours += f.ActualArrival.Sub(*f.ActualDeparture).Hours()
		}
		if f.ActualArrival != nil && f.ScheduledArrival != nil && f.ActualArrival.After(*f.ScheduledArrival) {
			stats.DelayMinutes += int(f.ActualArrival.Sub(*f.ScheduledArrival).Minutes())
		}
		distance := 0
		if f.DistanceMiles != nil {
			distance = *f.DistanceMiles
		}
		origin := s.airportPoint(r.Context(), points, f.Origin, f.OriginICAO)
		destination := s.airportPoint(r.Context(), points, f.Destination, f.DestinationICAO)
		if origin != nil && destination != nil {
			if distance <= 0 {
				distance = greatCircle(*origin, *destination)
			}
			segments = append(segments, passportSegment{f.Origin, f.Destination, origin.lat, origin.lon, destination.lat, destination.lon, distance})
		}
		stats.Miles += max(distance, 0)
	}
	stats.Hours = math.Round(stats.Hours*10) / 10
	stats.Aircraft = len(aircraft)
	respond(w, 200, map[string]any{"flights": flights, "segments": segments, "stats": stats})
	return nil
}

func (s Server) airportPoint(ctx context.Context, points map[string]*airportPoint, iata, icao string) *airportPoint {
	if s.Demo {
		return map[string]*airportPoint{"SFO": {37.62, -122.38}, "EWR": {40.69, -74.17}, "LHR": {51.47, -0.45}}[iata]
	}
	code := icao
	if code == "" {
		code = iata
	}
	if p, ok := points[code]; ok {
		return p
	}
	// Avoid unbounded network work for a single user history request.
	if code == "" || len(points) >= 10 || ctx.Err() != nil {
		return nil
	}
	var p *airportPoint
	if provider, ok := s.Source.(interface {
		Airport(context.Context, string) (feeds.Airport, error)
	}); ok {
		deadline, cancel := context.WithTimeout(ctx, 600*time.Millisecond)
		airport, err := provider.Airport(deadline, code)
		cancel()
		if err == nil && airport.Latitude != nil && airport.Longitude != nil {
			p = &airportPoint{*airport.Latitude, *airport.Longitude}
		}
	}
	points[code] = p
	return p
}

func greatCircle(from, to airportPoint) int {
	deg := math.Pi / 180
	lat, lon := (to.lat-from.lat)*deg, (to.lon-from.lon)*deg
	value := math.Pow(math.Sin(lat/2), 2) + math.Cos(from.lat*deg)*math.Cos(to.lat*deg)*math.Pow(math.Sin(lon/2), 2)
	return int(math.Round(2 * 3958.76 * math.Asin(math.Sqrt(min(1, max(0, value))))))
}

// PublicOrigin rejects URL components that would put access tokens on another host.
func PublicOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || u.Scheme != "http" && u.Scheme != "https" {
		return "", errInvalid
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}
