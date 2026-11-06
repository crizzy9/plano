package core

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

const (
	inboundForecastTTL  = 30 * time.Minute
	inboundReasonPrefix = "Inbound flight "
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
	features, reasons, inboundValidUntil, ok := delayInputs(flight, rotation, advisories, now)
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
	forecast.ValidUntil = inboundValidUntil
	departure := scheduled.Add(time.Duration(minutes * float64(time.Minute)))
	if end, validUntil, reason := departureClosureFloor(flight, advisories, now, departure); !end.IsZero() {
		if end.Sub(scheduled) > 24*time.Hour {
			return nil
		}
		departure = end
		if forecast.ValidUntil == nil || validUntil.Before(*forecast.ValidUntil) {
			forecast.ValidUntil = &validUntil
		}
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
	features, _, _, ok := delayInputs(flight, rotation, advisories, observedAt)
	return features, ok
}

func delayInputs(flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, now time.Time) (predict.Features, []string, *time.Time, bool) {
	var features predict.Features
	if flight.ScheduledDeparture == nil || flight.ScheduledDeparture.IsZero() || !flightBeforeDeparture(flight) {
		return features, nil, nil, false
	}
	scheduled := *flight.ScheduledDeparture
	reasons := make([]string, 0, 3)
	if flight.EstimatedDeparture != nil {
		features.AirlineDelayMinutes = max(0, flight.EstimatedDeparture.Sub(scheduled).Minutes())
	}
	var inboundValidUntil *time.Time
	if inbound, arrival := inboundLeg(flight, rotation, now); inbound != nil {
		ready := arrival.Add(40 * time.Minute)
		features.InboundDelayMinutes = max(0, ready.Sub(scheduled).Minutes())
		if features.InboundDelayMinutes > 0 {
			reasons = append(reasons, fmt.Sprintf(inboundReasonPrefix+"%s needs time to arrive and turn around.", inbound.Number))
			if inbound.ActualArrival == nil {
				inboundValidUntil = new(inbound.ObservedAt.Add(inboundForecastTTL))
			}
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
	return features, reasons, inboundValidUntil, true
}

func flightBeforeDeparture(flight feeds.Flight) bool {
	if flight.ActualDeparture != nil || flight.ActualTakeoff != nil || finished(flight) {
		return false
	}
	switch flight.Status {
	case "taxi_out", "airborne", "landed", "diverted":
		return false
	default:
		return true
	}
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

func inboundLeg(flight feeds.Flight, rotation []feeds.Flight, now time.Time) (*feeds.Flight, *time.Time) {
	if len(rotation) == 0 || flight.ScheduledDeparture == nil {
		return nil, nil
	}
	// The source returns a nearest-first aircraft chain. An earlier leg can
	// revisit this airport without being the one supplying the aircraft now.
	leg := &rotation[0]
	if leg.ID == "" || leg.ID == flight.ID || leg.ObservedAt.IsZero() || leg.ObservedAt.After(now) {
		return nil, nil
	}
	switch leg.Status {
	case "cancelled", "diverted", "unknown":
		return nil, nil
	}
	linked := flight.InboundID != "" && (leg.ID == flight.InboundID || leg.AeroProviderID() == flight.InboundID)
	tailsKnown := flight.TailNumber != "" && leg.TailNumber != ""
	if tailsKnown && !strings.EqualFold(flight.TailNumber, leg.TailNumber) || !linked && !tailsKnown {
		return nil, nil
	}
	airport := cmp.Or(leg.DestinationICAO, leg.Destination)
	sameAirport := airport != "" && airport == cmp.Or(flight.OriginICAO, flight.Origin)
	if leg.DestinationICAO == "" || flight.OriginICAO == "" {
		sameAirport = sameAirport || leg.Destination != "" && leg.Destination == flight.Origin
	}
	if !sameAirport || leg.ActualArrival == nil && !leg.ObservedAt.Add(inboundForecastTTL).After(now) ||
		leg.ActualArrival != nil && leg.ActualArrival.After(now) || leg.ActualLanding != nil && leg.ActualLanding.After(now) {
		return nil, nil
	}
	arrival := inboundArrival(*leg)
	scheduled := *flight.ScheduledDeparture
	if arrival == nil || arrival.IsZero() || arrival.Before(scheduled.Add(-25*time.Hour)) ||
		leg.ScheduledArrival != nil && leg.ScheduledArrival.After(scheduled) ||
		leg.ScheduledArrival == nil && !linked && arrival.After(scheduled) {
		return nil, nil
	}
	return leg, arrival
}

func inboundArrival(leg feeds.Flight) *time.Time {
	if leg.ActualArrival != nil {
		return leg.ActualArrival
	}
	// Wheels-on sets a lower bound when a gate ETA is missing or already past.
	if leg.ActualLanding != nil && (leg.EstimatedArrival == nil || leg.EstimatedArrival.Before(*leg.ActualLanding)) {
		return leg.ActualLanding
	}
	return firstTime(leg.EstimatedArrival, leg.ScheduledArrival)
}

func usesInboundTiming(forecast *Prediction) bool {
	return slices.ContainsFunc(forecast.Reasons, func(reason string) bool {
		return strings.HasPrefix(reason, inboundReasonPrefix)
	})
}

func predictionCurrent(forecast *Prediction, flight feeds.Flight, rotation []feeds.Flight, now time.Time) bool {
	if forecast.ValidUntil != nil && !forecast.ValidUntil.After(now) {
		return false
	}
	if usesInboundTiming(forecast) {
		// Databases created before inbound forecasts carried a validity window
		// still have the original reason and the cached rotation to check.
		leg, _ := inboundLeg(flight, rotation, now)
		return leg != nil
	}
	return true
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
