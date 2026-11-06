package core

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

// Prediction preserves the official schedule and labels the inputs behind an estimate.
type Prediction struct {
	Departure            *time.Time             `json:"departure,omitempty"`
	ValidUntil           *time.Time             `json:"validUntil,omitempty"`
	DelayMinutes         int                    `json:"delayMinutes"`
	Reasons              []string               `json:"reasons"`
	Method               string                 `json:"method"`
	ModelVersion         string                 `json:"modelVersion,omitempty"`
	BaselineMinutes      *float64               `json:"baselineMinutes,omitempty"`
	FeatureContributions []predict.Contribution `json:"featureContributions,omitempty"`
}

// Predict uses independent operational constraints as lower bounds, avoiding double
// counting when an airline's existing estimate already includes a disruption.
func Predict(flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, now time.Time) *Prediction {
	return PredictWithModel(nil, flight, rotation, advisories, now)
}

// PredictWithModel forecasts from observed flight and advisory signals when the
// trained model supports them. A missing model or an unsupported input falls back
// to the operational estimate. Neither estimate changes published flight times.
func PredictWithModel(model *predict.Model, flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, now time.Time) *Prediction {
	features, reasons, ok := delayInputs(flight, rotation, advisories, now)
	if !ok {
		return nil
	}
	scheduled := *flight.ScheduledDeparture
	minutes := max(features.AirlineDelayMinutes, features.InboundDelayMinutes, features.AirportDelayMinutes, features.GroundStopMinutes)
	forecast := Prediction{Method: "operational_estimate", DelayMinutes: int(minutes)}
	if estimate, ok := model.Estimate(features, now, scheduled); ok {
		minutes = estimate.DelayMinutes
		reasons = append(reasons, learnedReason(estimate.Contributions))
		forecast = Prediction{Method: "learned_estimate", DelayMinutes: int(math.Round(minutes)), ModelVersion: model.ID(),
			BaselineMinutes: &estimate.Baseline, FeatureContributions: estimate.Contributions}
	}
	departure := scheduled.Add(time.Duration(minutes * float64(time.Minute)))
	if end, validUntil, reason := departureClosureFloor(flight, advisories, now, departure); !end.IsZero() {
		if end.Sub(scheduled) > 24*time.Hour {
			return nil
		}
		departure = end
		forecast.ValidUntil = &validUntil
		forecast.DelayMinutes = int(math.Ceil(end.Sub(scheduled).Minutes()))
		reasons = append(reasons, reason)
	}
	if len(reasons) == 0 {
		return nil
	}
	forecast.Departure, forecast.Reasons = &departure, reasons
	return &forecast
}

// ExtractDelayFeatures gives historical snapshot exporters the same feature
// derivation used for live forecasts. Pass only data available at observedAt.
func ExtractDelayFeatures(flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, observedAt time.Time) (predict.Features, bool) {
	features, _, ok := delayInputs(flight, rotation, advisories, observedAt)
	return features, ok
}

func delayInputs(flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, now time.Time) (predict.Features, []string, bool) {
	var features predict.Features
	if flight.ScheduledDeparture == nil || flight.ScheduledDeparture.IsZero() || flight.ActualDeparture != nil || flight.ActualTakeoff != nil || finished(flight) {
		return features, nil, false
	}
	switch flight.Status {
	case "taxi_out", "airborne", "landed":
		return features, nil, false
	}
	scheduled := *flight.ScheduledDeparture
	reasons := make([]string, 0, 3)
	if flight.EstimatedDeparture != nil {
		features.AirlineDelayMinutes = max(0, flight.EstimatedDeparture.Sub(scheduled).Minutes())
	}
	if inbound := inboundLeg(flight, rotation, now); inbound != nil {
		ready := effectiveArrival(*inbound).Add(40 * time.Minute)
		features.InboundDelayMinutes = max(0, ready.Sub(scheduled).Minutes())
		if features.InboundDelayMinutes > 0 {
			reasons = append(reasons, fmt.Sprintf("Inbound flight %s needs time to arrive and turn around.", inbound.Number))
		}
	}
	for _, a := range advisories {
		// NOTAM text is airport context. A verified full closure constrains
		// the forecast separately, without changing trained model features.
		switch a.Kind {
		case "notam", "airport_closure", "airport_restriction", "runway_closure", "taxiway_closure", "apron_closure", "airport_operations",
			"cancelled", "canceled", "cancellation", "notam_cancelled", "notam_canceled", "notam_cancellation":
			continue
		}
		at := advisoryTime(flight, a)
		if at == nil || a.ObservedAt.After(now) {
			continue
		}
		if at.Before(now) {
			at = &now
		}
		if a.StartsAt != nil && a.StartsAt.After(*at) || a.EndsAt != nil && !a.EndsAt.After(*at) {
			continue
		}
		hasDelay := a.DelayMinutes != nil && *a.DelayMinutes > 0 && *a.DelayMinutes <= 1440
		if a.Reason != "" {
			reasons = append(reasons, fmt.Sprintf("%s reports %s at %s.", advisorySourceName(a.Source), a.Reason, a.Airport))
		} else if hasDelay || a.Kind == "ground_stop" && a.EndsAt != nil {
			reasons = append(reasons, fmt.Sprintf("%s reports an operational delay at %s.", advisorySourceName(a.Source), a.Airport))
		}
		if hasDelay {
			features.AirportDelayMinutes = max(features.AirportDelayMinutes, float64(*a.DelayMinutes))
		}
		if a.Kind == "ground_stop" && a.EndsAt != nil {
			features.GroundStopMinutes = max(features.GroundStopMinutes, a.EndsAt.Sub(scheduled).Minutes())
		}
		reason := strings.ToLower(a.Reason)
		features.Weather = features.Weather || containsAny(reason, "weather", "thunderstorm", "low visibility", "lightning", "wind", "fog", "snow", "deicing", "de-icing", "icing", "hail")
		features.AirTrafficControl = features.AirTrafficControl || containsAny(reason, "atc", "staffing", "airport volume", "capacity", "congestion", "traffic management")
	}
	return features, reasons, true
}

func departureClosureFloor(flight feeds.Flight, advisories []feeds.Advisory, now, departure time.Time) (time.Time, time.Time, string) {
	type closureWindow struct {
		start, end, freshUntil time.Time
		airport, source        string
	}
	var closures []closureWindow
	for _, a := range advisories {
		freshUntil := a.ObservedAt.Add(6 * time.Hour)
		if a.Kind != "airport_closure" || a.Direction != "" && a.Direction != "departure" ||
			a.Airport == "" || a.Airport != flight.Origin && a.Airport != flight.OriginICAO ||
			a.ObservedAt.IsZero() || !freshUntil.After(now) || a.ObservedAt.After(now) ||
			a.EndsAt == nil || !a.EndsAt.After(now) {
			continue
		}
		start := a.ObservedAt
		if a.StartsAt != nil {
			start = *a.StartsAt
		}
		if !a.EndsAt.After(start) {
			continue
		}
		summary, ok := feeds.DecodeNOTAM(a.RawReason)
		if !ok || !summary.FullAirportClosure ||
			summary.Airport != "" && summary.Airport != flight.Origin && summary.Airport != flight.OriginICAO {
			continue
		}
		// The decoder checks closure scope. An estimated or indefinite validity
		// marker, including in an ICAO C field, cannot set a departure floor.
		words := strings.FieldsFunc(strings.ToUpper(a.RawReason), func(r rune) bool { return r < 'A' || r > 'Z' })
		if slices.Contains(words, "EST") || slices.Contains(words, "PERM") || slices.Contains(words, "UFN") {
			continue
		}
		closures = append(closures, closureWindow{start: start, end: *a.EndsAt, freshUntil: freshUntil, airport: a.Airport, source: a.Source})
	}
	slices.SortStableFunc(closures, func(a, b closureWindow) int { return a.start.Compare(b.start) })
	if departure.Before(now) {
		departure = now
	}
	var last closureWindow
	validUntil := now.Add(30 * time.Minute)
	for _, closure := range closures {
		if closure.start.After(departure) {
			break
		}
		if closure.end.After(departure) {
			departure, last = closure.end, closure
			if closure.end.Before(validUntil) {
				validUntil = closure.end
			}
			if closure.freshUntil.Before(validUntil) {
				validUntil = closure.freshUntil
			}
		}
	}
	if last.end.IsZero() {
		return time.Time{}, time.Time{}, ""
	}
	return last.end, validUntil, fmt.Sprintf("%s reports a full airport closure at %s. The estimate allows for the published closure window.", advisorySourceName(last.source), last.airport)
}

func inboundLeg(flight feeds.Flight, rotation []feeds.Flight, now time.Time) *feeds.Flight {
	scheduled := *flight.ScheduledDeparture
	var inbound *feeds.Flight
	var selectedTime time.Time
	for i := range rotation {
		leg := &rotation[i]
		if leg.ID == flight.ID || leg.ObservedAt.After(now) || leg.Status == "cancelled" || leg.Status == "diverted" {
			continue
		}
		sameAirport := leg.Destination != "" && leg.Destination == flight.Origin
		if leg.DestinationICAO != "" && flight.OriginICAO != "" {
			sameAirport = leg.DestinationICAO == flight.OriginICAO
		}
		if !sameAirport || effectiveArrival(*leg) == nil {
			continue
		}
		arrives := firstTime(leg.ScheduledArrival, effectiveArrival(*leg))
		if arrives.After(scheduled) || arrives.Before(scheduled.Add(-25*time.Hour)) {
			continue
		}
		if inbound == nil || arrives.After(selectedTime) {
			inbound, selectedTime = leg, *arrives
		}
	}
	return inbound
}

func containsAny(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

func advisorySourceName(source string) string {
	switch source {
	case "faa_nas", feeds.FAASWIM:
		return "FAA"
	case feeds.Eurocontrol:
		return "EUROCONTROL"
	case "":
		return "An airport advisory"
	default:
		return source
	}
}

func learnedReason(contributions []predict.Contribution) string {
	labels := map[string]string{
		"airline_delay": "the airline's estimate", "inbound_turnaround": "inbound aircraft timing",
		"airport_delay": "reported airport delays", "ground_stop": "a ground stop", "weather": "weather reports",
		"air_traffic_control": "air traffic restrictions", "lead_time": "the time until departure",
	}
	var strongest predict.Contribution
	for _, contribution := range contributions {
		if _, ok := labels[contribution.Feature]; ok && contribution.Minutes > strongest.Minutes {
			strongest = contribution
		}
	}
	if strongest.Minutes < 1 {
		return "The forecast uses historical departures and the current flight signals."
	}
	return fmt.Sprintf("In the historical model, %s adds about %.0f min above the baseline.", labels[strongest.Feature], strongest.Minutes)
}

// advisoryTime uses the published operation direction and does not attribute
// area-wide advisories to a flight without route evidence.
func advisoryTime(flight feeds.Flight, a feeds.Advisory) *time.Time {
	if a.Airport == "" {
		return nil
	}
	if a.Direction != "departure" && (a.Airport == flight.Destination || a.Airport == flight.DestinationICAO) {
		// A destination ground stop holds an inbound flight at its origin.
		if a.Kind == "ground_stop" {
			if flight.ActualDeparture != nil || flight.ActualTakeoff != nil {
				return nil
			}
			return effectiveDeparture(flight)
		}
		return effectiveArrival(flight)
	}
	if a.Direction != "arrival" && (a.Airport == flight.Origin || a.Airport == flight.OriginICAO) {
		return effectiveDeparture(flight)
	}
	return nil
}
