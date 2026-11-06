package feeds

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// Rotation returns previous legs nearest-first. Explicit inbound links take
// precedence over a tail match, and every candidate must connect at the airport.
func (a *AeroAPI) Rotation(ctx context.Context, flight Flight) ([]Flight, error) {
	departure := rotationDeparture(flight)
	result := make([]Flight, 0)
	if departure == nil {
		return result, nil
	}
	cutoff := departure.Add(-rotationWindow)
	seen := map[string]bool{flight.ID: true, flight.providerID: true}
	leg := flight
	var tailRows []aeroFlight
	tailLoaded := false
	for len(result) < 24 {
		var candidate Flight
		if leg.InboundID != "" && !seen[leg.InboundID] {
			inbound, err := a.getProviderFlight(ctx, leg.InboundID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return result, err
			}
			if err == nil && rotationConnects(inbound, leg, cutoff) {
				candidate = inbound
			}
		}
		if candidate.ID == "" && providerIdentifier.MatchString(flight.TailNumber) {
			if !tailLoaded {
				tailLoaded = true
				params := url.Values{"ident_type": {"registration"}, "max_pages": {"4"}}
				path := "/flights/"
				now := a.now().UTC()
				start := cutoff.Add(-24 * time.Hour)
				switch {
				case departure.Before(now.Add(-10 * 24 * time.Hour)):
					path = "/history/flights/"
					params.Set("start", start.Format(time.RFC3339))
					params.Set("end", departure.Format(time.RFC3339))
				default:
					if !start.Before(now.Add(-10 * 24 * time.Hour)) {
						params.Set("start", start.Format(time.RFC3339))
					}
					if departure.Before(now.Add(48 * time.Hour)) {
						params.Set("end", departure.Format(time.RFC3339))
					}
				}
				rows, err := a.flightRows(ctx, path+flight.TailNumber, params)
				if err != nil && !errors.Is(err, ErrNotFound) {
					return result, err
				}
				tailRows = rows
			}
			for _, row := range tailRows {
				if row.Blocked || seen[row.ID] || !providerIdentifier.MatchString(row.ID) ||
					!strings.EqualFold(row.Registration, flight.TailNumber) {
					continue
				}
				possible := row.normalize(a.now().UTC())
				if rotationConnects(possible, leg, cutoff) &&
					(candidate.ID == "" || rotationArrival(possible).After(*rotationArrival(candidate))) {
					candidate = possible
				}
			}
		}
		if candidate.ID == "" {
			break
		}
		seen[candidate.ID] = true
		result = append(result, candidate)
		leg = candidate
	}
	return result, nil
}

func rotationConnects(inbound, next Flight, cutoff time.Time) bool {
	if inbound.Status == "cancelled" || inbound.Status == "diverted" || inbound.Status == "unknown" {
		return false
	}
	if inbound.TailNumber != "" && next.TailNumber != "" && !strings.EqualFold(inbound.TailNumber, next.TailNumber) {
		return false
	}
	if first(inbound.DestinationICAO, inbound.Destination) == "" {
		return false
	}
	sameAirport := first(inbound.DestinationICAO, inbound.Destination) == first(next.OriginICAO, next.Origin)
	if inbound.DestinationICAO == "" || next.OriginICAO == "" {
		sameAirport = sameAirport || (inbound.Destination != "" && inbound.Destination == next.Origin)
	}
	departure := rotationDeparture(next)
	inboundDeparture := rotationDeparture(inbound)
	arrival := rotationArrival(inbound)
	scheduledArrival := inbound.ScheduledArrival
	if scheduledArrival == nil {
		scheduledArrival = arrival
	}
	// The current inbound estimate can exceed the outbound time, which is
	// precisely the cascading delay the caller needs to observe.
	return sameAirport && arrival != nil && !arrival.Before(cutoff) &&
		departure != nil && inboundDeparture != nil && inboundDeparture.Before(*departure) &&
		scheduledArrival != nil && !scheduledArrival.After(*departure)
}

func rotationDeparture(flight Flight) *time.Time {
	return firstTime(flight.ScheduledDeparture, flight.ActualDeparture, flight.EstimatedDeparture, flight.ActualTakeoff)
}

func rotationArrival(flight Flight) *time.Time {
	return firstTime(flight.ActualArrival, flight.EstimatedArrival, flight.ScheduledArrival, flight.ActualLanding)
}

func firstTime(times ...*time.Time) *time.Time {
	for _, instant := range times {
		if instant != nil {
			return instant
		}
	}
	return nil
}
