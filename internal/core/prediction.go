package core

import (
	"fmt"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

// Prediction preserves the official schedule and labels the inputs behind an estimate.
type Prediction struct {
	Departure    *time.Time `json:"departure,omitempty"`
	DelayMinutes int        `json:"delayMinutes"`
	Reasons      []string   `json:"reasons"`
	Method       string     `json:"method"`
}

// Predict uses independent operational constraints as lower bounds, avoiding double
// counting when an airline's existing estimate already includes a disruption.
func Predict(flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, now time.Time) *Prediction {
	if flight.ScheduledDeparture == nil || flight.ActualDeparture != nil || finished(flight) {
		return nil
	}
	scheduled := *flight.ScheduledDeparture
	lower := scheduled
	reasons := make([]string, 0, 3)
	if flight.EstimatedDeparture != nil && flight.EstimatedDeparture.After(lower) {
		lower = *flight.EstimatedDeparture
	}
	var inbound *feeds.Flight
	for i := range rotation {
		leg := &rotation[i]
		if leg.ID == flight.ID || leg.Destination != flight.Origin || leg.Status == "cancelled" {
			continue
		}
		arrives := firstTime(leg.ScheduledArrival, effectiveArrival(*leg))
		if arrives == nil || arrives.After(scheduled) || arrives.Before(scheduled.Add(-25*time.Hour)) {
			continue
		}
		if inbound == nil || arrives.After(*firstTime(inbound.ScheduledArrival, effectiveArrival(*inbound))) {
			inbound = leg
		}
	}
	if inbound != nil {
		ready := effectiveArrival(*inbound).Add(40 * time.Minute)
		if ready.After(scheduled) {
			reasons = append(reasons, fmt.Sprintf("Inbound flight %s needs time to arrive and turn around.", inbound.Number))
			if ready.After(lower) {
				lower = ready
			}
		}
	}
	for _, a := range advisories {
		at := advisoryTime(flight, a)
		if at == nil || a.Kind == "airport_restriction" {
			continue
		}
		if at.Before(now) {
			at = &now
		}
		if a.StartsAt != nil && a.StartsAt.After(*at) || a.EndsAt != nil && !a.EndsAt.After(*at) {
			continue
		}
		if a.Reason != "" {
			reasons = append(reasons, fmt.Sprintf("%s reports %s at %s.", a.Source, a.Reason, a.Airport))
		}
		if a.DelayMinutes != nil && *a.DelayMinutes > 0 && *a.DelayMinutes <= 1440 {
			lower = maxTime(lower, scheduled.Add(time.Duration(*a.DelayMinutes)*time.Minute))
		}
		if a.Kind == "ground_stop" && a.EndsAt != nil {
			lower = maxTime(lower, *a.EndsAt)
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	return &Prediction{Departure: &lower, DelayMinutes: int(lower.Sub(scheduled).Minutes()), Reasons: reasons, Method: "operational_estimate"}
}

func maxTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
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
