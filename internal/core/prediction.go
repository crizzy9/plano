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
	inboundForecastTTL     = 30 * time.Minute
	inboundReasonPrefix    = "Inbound flight "
	departureForecastLimit = 24 * time.Hour
)

// Prediction preserves the official schedule and labels the inputs behind an estimate.
type Prediction struct {
	Departure               *time.Time               `json:"departure,omitempty"`
	ValidUntil              *time.Time               `json:"validUntil,omitempty"`
	DelayMinutes            int                      `json:"delayMinutes"`
	Reasons                 []string                 `json:"reasons"`
	Method                  string                   `json:"method"`
	ModelVersion            string                   `json:"modelVersion,omitempty"`
	BaselineMinutes         *float64                 `json:"baselineMinutes,omitempty"`
	FeatureContributions    []predict.Contribution   `json:"featureContributions,omitempty"`
	AdvisoryEvidence        []forecastAdvisory       `json:"advisoryEvidence,omitempty"`
	TerminalWeatherEvidence *terminalWeatherEvidence `json:"terminalWeatherEvidence,omitempty"`
}

type forecastInputs struct {
	features   predict.Features
	reasons    []string
	validUntil *time.Time
	advisories []forecastAdvisory
}

func (s *Store) delayModelFor(flight feeds.Flight, now time.Time) *predict.Model {
	if s.earlyDelayModel != nil && flight.ScheduledDeparture != nil && flight.ScheduledDeparture.Sub(now) > earlyDelayBoundary {
		return s.earlyDelayModel
	}
	return s.delayModel
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
	return predictWithTerminalWeather(model, flight, rotation, advisories, nil, now)
}

func predictWithTerminalWeather(model *predict.Model, flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, forecasts []AirportWeatherForecast, now time.Time) *Prediction {
	inputs, ok := delayInputs(flight, rotation, advisories, now)
	if !ok {
		return nil
	}
	features, reasons := inputs.features, inputs.reasons
	var weather []AirportWeatherForecast
	if model.UsesTerminalWeatherForecast() {
		weather = prevailingTerminalWeather(forecasts)
		features.TerminalWeatherForecast = len(weather) > 0
	}
	scheduled := *flight.ScheduledDeparture
	minutes := max(features.AirlineDelayMinutes, features.InboundDelayMinutes, features.AirportDelayMinutes, features.GroundStopMinutes)
	forecast := Prediction{Method: "operational_estimate", DelayMinutes: int(minutes)}
	if estimate, ok := model.Estimate(features, now, scheduled); ok {
		minutes = estimate.DelayMinutes
		reasons = append(reasons, learnedReason(features, estimate.Contributions))
		forecast = Prediction{Method: "learned_estimate", DelayMinutes: int(math.Round(minutes)), ModelVersion: model.ID(),
			BaselineMinutes: &estimate.Baseline, FeatureContributions: estimate.Contributions}
		if model.UsesTerminalWeatherForecast() {
			forecast.TerminalWeatherEvidence = &terminalWeatherEvidence{Forecasts: weather}
			for _, period := range weather {
				reasons = append(reasons, fmt.Sprintf("NOAA forecasts %s at %s during the expected %s.", period.Conditions, period.Airport, period.Operation))
				inputs.validUntil = earliestValidity(inputs.validUntil, period.EndsAt)
			}
		}
	}
	forecast.ValidUntil = inputs.validUntil
	forecast.AdvisoryEvidence = inputs.advisories
	departure := scheduled.Add(time.Duration(minutes * float64(time.Minute)))
	statusAt := flight.ObservedAt
	if statusAt.After(now) {
		statusAt = now
	}
	if elapsed := predict.ElapsedDepartureDelay(scheduled, statusAt); elapsed > minutes {
		departure = statusAt
		forecast.DelayMinutes = int(math.Ceil(elapsed))
		if len(reasons) != 0 {
			reasons = append(reasons, "The flight was still awaiting gate-out at the last flight update.")
		}
	}
	// Turnaround, model inference or an earlier closure can move the expected
	// departure into a later operational hold.
	if hold := departureHoldFloor(flight, advisories, now, departure); !hold.end.IsZero() {
		if hold.end.Sub(scheduled) > departureForecastLimit {
			return nil
		}
		departure = hold.end
		forecast.ValidUntil = earliestValidity(forecast.ValidUntil, hold.validUntil)
		forecast.AdvisoryEvidence = append(forecast.AdvisoryEvidence, hold.advisories...)
		forecast.DelayMinutes = int(math.Ceil(hold.end.Sub(scheduled).Minutes()))
		reasons = append(reasons, hold.reasons...)
	}
	if len(reasons) == 0 {
		return nil
	}
	forecast.Departure, forecast.Reasons = &departure, reasons
	return &forecast
}

// ExtractDelayFeatures derives the flight and advisory features for historical
// snapshot exporters. Pass only data available at observedAt.
func ExtractDelayFeatures(flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, observedAt time.Time) (predict.Features, bool) {
	return extractDelayFeaturesWithTerminalWeather(flight, rotation, advisories, nil, observedAt)
}

func extractDelayFeaturesWithTerminalWeather(flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, forecasts []AirportWeatherForecast, observedAt time.Time) (predict.Features, bool) {
	inputs, ok := delayInputs(flight, rotation, advisories, observedAt)
	inputs.features.TerminalWeatherForecast = slices.ContainsFunc(forecasts, isPrevailingTerminalWeather)
	return inputs.features, ok
}

func delayInputs(flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, now time.Time) (forecastInputs, bool) {
	var inputs forecastInputs
	if flight.ScheduledDeparture == nil || flight.ScheduledDeparture.IsZero() || !flightBeforeDeparture(flight) {
		return inputs, false
	}
	features := &inputs.features
	scheduled := *flight.ScheduledDeparture
	reasons := make([]string, 0, 3)
	if flight.EstimatedDeparture != nil {
		features.AirlineDelayMinutes = max(0, flight.EstimatedDeparture.Sub(scheduled).Minutes())
	}
	if inbound, arrival, validUntil := inboundLeg(flight, rotation, now); inbound != nil {
		ready := arrival.Add(40 * time.Minute)
		features.InboundDelayMinutes = max(0, ready.Sub(scheduled).Minutes())
		if features.InboundDelayMinutes > 0 {
			reasons = append(reasons, fmt.Sprintf(inboundReasonPrefix+"%s needs time to arrive and turn around.", inbound.Number))
			inputs.validUntil = validUntil
		}
	}
	for _, a := range advisories {
		if a.AirportContextOnly {
			continue
		}
		// NOTAM text is airport context. A verified full closure constrains
		// the forecast separately, without changing trained model features.
		switch a.Kind {
		case "notam", feeds.AdvisoryAirportNotice, "airport_closure", "airport_restriction", "runway_closure", "taxiway_closure", "apron_closure", "airport_operations":
			continue
		}
		at := advisoryTime(flight, a)
		validUntil := advisoryForecastValidity(a, now)
		if at == nil || !validUntil.After(now) {
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
		} else {
			continue
		}
		inputs.validUntil = earliestValidity(inputs.validUntil, validUntil)
		inputs.advisories = append(inputs.advisories, advisoryEvidence(a))
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
	inputs.reasons = reasons
	return inputs, true
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

type departureHoldAdjustment struct {
	end, validUntil time.Time
	reasons         []string
	advisories      []forecastAdvisory
}

func departureHoldFloor(flight feeds.Flight, advisories []feeds.Advisory, now, departure time.Time) departureHoldAdjustment {
	type holdWindow struct {
		start, end, freshUntil time.Time
		kind, airport, source  string
		reason                 string
		evidence               forecastAdvisory
	}
	if departure.Before(now) {
		departure = now
	}
	horizon := flight.ScheduledDeparture.Add(departureForecastLimit)
	if horizon.Before(departure) {
		horizon = departure
	}
	var windows []holdWindow
	for _, a := range advisories {
		if a.AirportContextOnly {
			continue
		}
		if a.Kind == "ground_stop" {
			freshUntil := advisoryForecastValidity(a, now)
			if a.EndsAt == nil || advisoryTime(flight, a) == nil || !freshUntil.After(now) {
				continue
			}
			start := firstTime(a.StartsAt, &a.ObservedAt)
			windows = append(windows, holdWindow{start: *start, end: *a.EndsAt, freshUntil: freshUntil,
				kind: a.Kind, airport: a.Airport, source: a.Source, reason: a.Reason, evidence: advisoryEvidence(a)})
			continue
		}
		if a.Kind != "airport_closure" || a.Direction != "" && a.Direction != "departure" ||
			a.Airport == "" || a.Airport != flight.Origin && a.Airport != flight.OriginICAO {
			continue
		}
		summary, ok := feeds.DecodeNOTAM(a.RawReason)
		if !ok || !summary.FullAirportClosure ||
			summary.Airport != "" && summary.Airport != flight.Origin && summary.Airport != flight.OriginICAO {
			continue
		}
		// Older bridge records may still be missing structured validity. Keep
		// their original state as evidence so revision checks compare like data.
		timed := a
		if summary.HasValidity {
			if !summary.MatchesValidity(a.StartsAt, a.EndsAt) {
				continue
			}
			timed.StartsAt, timed.EndsAt = summary.StartsAt, summary.EndsAt
		}
		freshUntil := advisoryForecastValidity(timed, now)
		if !freshUntil.After(now) || timed.EndsAt == nil {
			continue
		}
		window := holdWindow{freshUntil: freshUntil, kind: a.Kind, airport: a.Airport, source: a.Source, evidence: advisoryEvidence(a)}
		if summary.HasDailyClosure() {
			// Only expand occurrences that the closure chain can reach before
			// the forecast limit, regardless of the full notice's duration.
			for at := departure; !at.After(horizon); {
				start, end, ok := summary.NextDailyClosure(at)
				if !ok || start.After(horizon) {
					break
				}
				window.start, window.end = start, end
				window.freshUntil = earliestTime(freshUntil, end)
				windows = append(windows, window)
				at = end
			}
			continue
		}
		start := timed.ObservedAt
		if timed.StartsAt != nil {
			start = *timed.StartsAt
		}
		if !timed.EndsAt.After(start) {
			continue
		}
		window.start, window.end = start, *timed.EndsAt
		windows = append(windows, window)
	}
	slices.SortStableFunc(windows, func(a, b holdWindow) int { return a.start.Compare(b.start) })
	var lastClosure, lastStop holdWindow
	adjustment := departureHoldAdjustment{validUntil: now.Add(advisoryForecastTTL)}
	applied := make(map[forecastAdvisory]bool)
	for _, window := range windows {
		if window.start.After(departure) {
			break
		}
		if window.end.After(departure) {
			departure, adjustment.end = window.end, window.end
			adjustment.validUntil = earliestTime(adjustment.validUntil, window.freshUntil)
			if window.kind == "ground_stop" {
				lastStop = window
			} else {
				lastClosure = window
			}
			if !applied[window.evidence] {
				adjustment.advisories = append(adjustment.advisories, window.evidence)
				applied[window.evidence] = true
			}
		}
	}
	if !lastClosure.end.IsZero() {
		adjustment.reasons = append(adjustment.reasons, fmt.Sprintf("%s reports a full airport closure at %s. The estimate allows for the published closure window.",
			advisorySourceName(lastClosure.source), lastClosure.airport))
	}
	if !lastStop.end.IsZero() {
		reason := fmt.Sprintf("%s reports a ground stop at %s", advisorySourceName(lastStop.source), lastStop.airport)
		if lastStop.reason != "" {
			reason += " due to " + lastStop.reason
		}
		adjustment.reasons = append(adjustment.reasons, reason+".")
	}
	return adjustment
}

func inboundLeg(flight feeds.Flight, rotation []feeds.Flight, now time.Time) (leg *feeds.Flight, arrival, validUntil *time.Time) {
	if len(rotation) == 0 || flight.ScheduledDeparture == nil {
		return nil, nil, nil
	}
	// The source returns a nearest-first aircraft chain. An earlier leg can
	// revisit this airport without being the one supplying the aircraft now.
	leg = &rotation[0]
	if leg.ID == "" || leg.ID == flight.ID || leg.ObservedAt.IsZero() || leg.ObservedAt.After(now) {
		return nil, nil, nil
	}
	switch leg.Status {
	case "cancelled", "diverted", "unknown":
		return nil, nil, nil
	}
	linked := flight.InboundID != "" && (leg.ID == flight.InboundID || leg.AeroProviderID() == flight.InboundID)
	tailsKnown := flight.TailNumber != "" && leg.TailNumber != ""
	if tailsKnown && !strings.EqualFold(flight.TailNumber, leg.TailNumber) || !linked && !tailsKnown {
		return nil, nil, nil
	}
	airport := cmp.Or(leg.DestinationICAO, leg.Destination)
	sameAirport := airport != "" && airport == cmp.Or(flight.OriginICAO, flight.Origin)
	if leg.DestinationICAO == "" || flight.OriginICAO == "" {
		sameAirport = sameAirport || leg.Destination != "" && leg.Destination == flight.Origin
	}
	if !sameAirport ||
		leg.ActualArrival != nil && leg.ActualArrival.After(now) || leg.ActualLanding != nil && leg.ActualLanding.After(now) {
		return nil, nil, nil
	}
	arrival, estimated := inboundArrival(*leg)
	scheduled := *flight.ScheduledDeparture
	if arrival == nil || arrival.IsZero() || arrival.Before(scheduled.Add(-25*time.Hour)) ||
		leg.ScheduledArrival != nil && leg.ScheduledArrival.After(scheduled) ||
		leg.ScheduledArrival == nil && !linked && arrival.After(scheduled) ||
		!inboundChronologyCompatible(*leg, *arrival, scheduled) {
		return nil, nil, nil
	}
	if leg.ActualArrival != nil {
		return leg, arrival, nil
	}
	validUntil = new(leg.ObservedAt.Add(inboundForecastTTL))
	if estimated {
		validUntil = earliestValidity(validUntil, *arrival)
	}
	if !validUntil.After(now) {
		return nil, nil, nil
	}
	return leg, arrival, validUntil
}

func inboundChronologyCompatible(leg feeds.Flight, arrival, onwardDeparture time.Time) bool {
	departure := firstTime(leg.ScheduledDeparture, leg.ActualDeparture, leg.ActualTakeoff, leg.EstimatedDeparture)
	if departure != nil && (!departure.Before(onwardDeparture) || arrival.Before(*departure)) {
		return false
	}
	actualDeparture := firstTime(leg.ActualDeparture, leg.ActualTakeoff)
	return actualDeparture == nil || !arrival.Before(*actualDeparture)
}

func inboundArrival(leg feeds.Flight) (arrival *time.Time, estimated bool) {
	if leg.ActualArrival != nil {
		return leg.ActualArrival, false
	}
	// A reported wheels-on time bounds a missing or contradicted gate ETA.
	if leg.ActualLanding != nil && (leg.EstimatedArrival == nil || leg.EstimatedArrival.Before(*leg.ActualLanding)) {
		return leg.ActualLanding, false
	}
	return firstTime(leg.EstimatedArrival, leg.ScheduledArrival), true
}

func usesInboundTiming(forecast *Prediction) bool {
	return slices.ContainsFunc(forecast.Reasons, func(reason string) bool {
		return strings.HasPrefix(reason, inboundReasonPrefix)
	})
}

func predictionValidity(forecast *Prediction, flight feeds.Flight, rotation []feeds.Flight, now time.Time) (validUntil *time.Time, valid bool) {
	if forecast == nil || forecast.ValidUntil != nil && !forecast.ValidUntil.After(now) {
		return nil, false
	}
	if forecast.Departure != nil && flightBeforeDeparture(flight) &&
		forecast.Departure.Before(flight.ObservedAt) && forecast.Departure.Before(now) {
		return nil, false
	}
	if len(forecast.AdvisoryEvidence) == 0 && slices.ContainsFunc(forecast.Reasons, legacyAdvisoryReason) {
		return nil, false
	}
	validUntil = forecast.ValidUntil
	if usesInboundTiming(forecast) {
		// Legacy forecasts can outlive an ETA in their stored validity window.
		leg, _, inboundUntil := inboundLeg(flight, rotation, now)
		if leg == nil {
			return nil, false
		}
		if inboundUntil != nil {
			validUntil = earliestValidity(validUntil, *inboundUntil)
		}
	}
	return validUntil, true
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

func learnedReason(features predict.Features, contributions []predict.Contribution) string {
	signals := map[string]struct {
		label    string
		observed bool
	}{
		"airline_delay":             {"the airline's estimate", features.AirlineDelayMinutes > 0},
		"inbound_turnaround":        {"inbound aircraft timing", features.InboundDelayMinutes > 0},
		"airport_delay":             {"reported airport delays", features.AirportDelayMinutes > 0},
		"ground_stop":               {"a ground stop", features.GroundStopMinutes > 0},
		"weather":                   {"weather reports", features.Weather},
		"terminal_weather_forecast": {"forecast airport weather", features.TerminalWeatherForecast},
		"air_traffic_control":       {"air traffic restrictions", features.AirTrafficControl},
		"lead_time":                 {"the time until departure", true},
	}
	var strongest predict.Contribution
	for _, contribution := range contributions {
		if signals[contribution.Feature].observed && contribution.Minutes > strongest.Minutes {
			strongest = contribution
		}
	}
	if strongest.Minutes < 1 {
		return "The forecast uses historical departures and the current flight signals."
	}
	return fmt.Sprintf("In the historical model, %s adds about %.0f min above the baseline.", signals[strongest.Feature].label, strongest.Minutes)
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
