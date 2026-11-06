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
	tail := first(flight.TailNumber)
	var tailRows []Flight
	tailLoaded := false
	var requestErr error
	for len(result) < 24 {
		if err := ctx.Err(); err != nil {
			requestErr = err
			break
		}
		var candidate Flight
		var rejectedID string
		var latestIntervening time.Time
		if requestErr == nil && providerIdentifier.MatchString(leg.InboundID) && !seen[leg.InboundID] {
			inbound, err := a.getProviderFlight(ctx, leg.InboundID)
			switch {
			case err == nil && rotationTailMatches(inbound, tail) && rotationAirportMatches(inbound, leg) && rotationPrecedes(inbound, leg, cutoff, true):
				candidate = inbound
			case err == nil:
				rejectedID = inbound.ID
				if tail != "" && strings.EqualFold(tail, inbound.TailNumber) {
					if movement := rotationInterveningDeparture(inbound, leg, cutoff); movement != nil {
						latestIntervening = *movement
					}
				}
			case !errors.Is(err, ErrNotFound):
				requestErr = err
			}
			if err := ctx.Err(); err != nil {
				requestErr = err
				break
			}
		}
		if candidate.ID == "" && providerIdentifier.MatchString(tail) {
			if !tailLoaded && requestErr == nil {
				tailLoaded = true
				rows, err := a.rotationTailRows(ctx, tail, *departure, cutoff)
				if err != nil && !errors.Is(err, ErrNotFound) {
					requestErr = err
				}
				tailRows = rows
			}
			candidate = rotationTailCandidate(tailRows, leg, cutoff, seen, rejectedID, latestIntervening)
		}
		if candidate.ID == "" {
			break
		}
		seen[candidate.ID] = true
		result = append(result, candidate)
		leg = candidate
		tail = first(tail, candidate.TailNumber)
	}
	if len(result) == 0 && requestErr != nil {
		return nil, requestErr
	}
	return result, requestErr
}

func (a *AeroAPI) rotationTailRows(ctx context.Context, tail string, departure, cutoff time.Time) ([]Flight, error) {
	params := url.Values{"ident_type": {"registration"}, "max_pages": {"4"}}
	path := "/flights/"
	now := a.now().UTC()
	start := cutoff.Add(-24 * time.Hour)
	if departure.Before(now.Add(-10 * 24 * time.Hour)) {
		path = "/history/flights/"
		params.Set("start", start.Format(time.RFC3339))
		params.Set("end", departure.Format(time.RFC3339))
	} else {
		if !start.Before(now.Add(-10 * 24 * time.Hour)) {
			params.Set("start", start.Format(time.RFC3339))
		}
		if departure.Before(now.Add(48 * time.Hour)) {
			params.Set("end", departure.Format(time.RFC3339))
		}
	}
	rows, err := a.flightRows(ctx, path+tail, params)
	if err != nil {
		return nil, err
	}
	// A registration lookup can include several rows for one provider ID.
	// A blocked or disrupted row disqualifies that ID across the response.
	disqualified := make(map[string]bool)
	flights := make([]Flight, 0, len(rows))
	for _, row := range rows {
		if row.Blocked || row.Cancelled || row.Diverted {
			disqualified[row.ID] = true
			continue
		}
		if providerIdentifier.MatchString(row.ID) && strings.EqualFold(row.Registration, tail) {
			flight := row.normalize(now)
			if flight.Status == "cancelled" || flight.Status == "diverted" {
				disqualified[row.ID] = true
				continue
			}
			flights = append(flights, flight)
		}
	}
	kept := flights[:0]
	for _, flight := range flights {
		if !disqualified[flight.ID] {
			kept = append(kept, flight)
		}
	}
	return kept, nil
}

func rotationTailCandidate(rows []Flight, next Flight, cutoff time.Time, seen map[string]bool, rejectedID string, latestIntervening time.Time) Flight {
	var candidate Flight
	for _, row := range rows {
		if seen[row.ID] {
			continue
		}
		if movement := rotationInterveningDeparture(row, next, cutoff); movement != nil && movement.After(latestIntervening) {
			latestIntervening = *movement
		}
		if row.ID == rejectedID {
			continue
		}
		linked := row.ID == next.InboundID
		if !rotationPrecedes(row, next, cutoff, linked) {
			continue
		}
		if linked && rotationAirportMatches(row, next) {
			return row
		}
		if candidate.ID == "" || rotationMoreRecent(row, candidate) {
			candidate = row
		}
	}
	// A more recent move to another airport breaks the inferred chain.
	if rotationAirportMatches(candidate, next) && latestIntervening.Before(*rotationMovement(candidate)) {
		return candidate
	}
	return Flight{}
}

func rotationInterveningDeparture(inbound, next Flight, cutoff time.Time) *time.Time {
	departed := firstTime(inbound.ActualDeparture, inbound.ActualTakeoff)
	if departed == nil || departed.Before(cutoff) || departed.After(inbound.ObservedAt) ||
		first(inbound.DestinationICAO, inbound.Destination) == "" || rotationAirportMatches(inbound, next) {
		return nil
	}
	if nextDeparture := firstTime(next.ActualDeparture, next.ActualTakeoff); nextDeparture != nil && !departed.Before(*nextDeparture) {
		return nil
	}
	return departed
}

func rotationMoreRecent(a, b Flight) bool {
	aDeparture, bDeparture := rotationMovement(a), rotationMovement(b)
	if !aDeparture.Equal(*bDeparture) {
		return aDeparture.After(*bDeparture)
	}
	aArrival := firstTime(a.ScheduledArrival, a.ActualArrival, a.ActualLanding, a.EstimatedArrival)
	bArrival := firstTime(b.ScheduledArrival, b.ActualArrival, b.ActualLanding, b.EstimatedArrival)
	if !aArrival.Equal(*bArrival) {
		return aArrival.After(*bArrival)
	}
	return a.ID < b.ID
}

func rotationTailMatches(inbound Flight, tail string) bool {
	return tail == "" || inbound.TailNumber == "" || strings.EqualFold(inbound.TailNumber, tail)
}

func rotationAirportMatches(inbound, next Flight) bool {
	destination := first(inbound.DestinationICAO, inbound.Destination)
	if destination == "" {
		return false
	}
	if destination == first(next.OriginICAO, next.Origin) {
		return true
	}
	return (inbound.DestinationICAO == "" || next.OriginICAO == "") && inbound.Destination != "" && inbound.Destination == next.Origin
}

func rotationPrecedes(inbound, next Flight, cutoff time.Time, linked bool) bool {
	if inbound.Status == "cancelled" || inbound.Status == "diverted" || inbound.Status == "unknown" {
		return false
	}
	arrival := rotationArrival(inbound)
	departure := rotationDeparture(next)
	inboundDeparture := rotationDeparture(inbound)
	if arrival == nil || arrival.Before(cutoff) || departure == nil || inboundDeparture == nil ||
		!inboundDeparture.Before(*departure) || arrival.Before(*inboundDeparture) {
		return false
	}
	if actualDeparture := firstTime(next.ActualDeparture, next.ActualTakeoff); actualDeparture != nil && arrival.After(*actualDeparture) {
		return false
	}
	if actualInboundDeparture := firstTime(inbound.ActualDeparture, inbound.ActualTakeoff); actualInboundDeparture != nil && arrival.Before(*actualInboundDeparture) {
		return false
	}
	if inbound.ScheduledArrival != nil {
		return !inbound.ScheduledArrival.After(*departure)
	}
	// An explicit previous-flight ID can establish the link when a late ETA
	// is the only arrival time. A tail match alone needs an earlier arrival.
	return linked || !arrival.After(*departure)
}

func rotationDeparture(flight Flight) *time.Time {
	return firstTime(flight.ScheduledDeparture, flight.ActualDeparture, flight.ActualTakeoff, flight.EstimatedDeparture)
}

func rotationMovement(flight Flight) *time.Time {
	return firstTime(flight.ActualDeparture, flight.ActualTakeoff, flight.ScheduledDeparture, flight.EstimatedDeparture)
}

func rotationArrival(flight Flight) *time.Time {
	if flight.ActualArrival != nil {
		return flight.ActualArrival
	}
	arrival := firstTime(flight.EstimatedArrival, flight.ActualLanding, flight.ScheduledArrival)
	if flight.ActualLanding != nil && flight.ActualLanding.After(*arrival) {
		return flight.ActualLanding
	}
	return arrival
}

func firstTime(times ...*time.Time) *time.Time {
	for _, instant := range times {
		if instant != nil {
			return instant
		}
	}
	return nil
}
