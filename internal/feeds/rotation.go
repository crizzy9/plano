package feeds

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

const maxRotationLegs = 24

type rotationLookupInterrupted struct{ error }

func (e rotationLookupInterrupted) Unwrap() error { return e.error }

// RotationSuffixRecoverable reports whether an explicit predecessor lookup
// interrupted a verified prefix before other evidence could contradict it.
func RotationSuffixRecoverable(err error) bool {
	var interrupted rotationLookupInterrupted
	return errors.As(err, &interrupted)
}

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
	for len(result) < maxRotationLegs {
		explicitLookup := providerIdentifier.MatchString(leg.InboundID) && !seen[leg.InboundID]
		if err := ctx.Err(); err != nil {
			if requestErr == nil && len(result) != 0 && !tailLoaded && explicitLookup {
				return result, rotationLookupInterrupted{err}
			}
			requestErr = err
			break
		}
		var candidate Flight
		var rejectedID string
		var latestIntervening time.Time
		if requestErr == nil && explicitLookup {
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
				// Registration rows can contain newer movements that would
				// contradict a saved suffix. Let the normal scan decide when
				// those rows are available.
				if len(result) != 0 && !tailLoaded &&
					(errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
					return result, rotationLookupInterrupted{err}
				}
			}
			if err := ctx.Err(); err != nil && candidate.ID == "" {
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

// RestoreRotationSuffix fills an interrupted explicit lookup from the saved
// chain when the returned prefix still identifies the same movements. The
// caller checks that the outbound scope and interruption allow recovery.
func RestoreRotationSuffix(flight Flight, prefix, saved []Flight) []Flight {
	departure := rotationDeparture(flight)
	if departure == nil || len(prefix) == 0 || len(prefix) >= min(len(saved), maxRotationLegs) {
		return prefix
	}
	cutoff := departure.Add(-rotationWindow)
	seen := map[string]bool{flight.ID: true, flight.AeroProviderID(): true}
	next, tail := flight, flight.TailNumber
	for i, leg := range prefix {
		cached := saved[i]
		if leg.ID != cached.ID || !providerIdentifier.MatchString(leg.ID) || seen[leg.ID] ||
			!rotationSameAirport(leg.Origin, leg.OriginICAO, cached.Origin, cached.OriginICAO) ||
			!rotationSameAirport(leg.Destination, leg.DestinationICAO, cached.Destination, cached.DestinationICAO) ||
			!rotationTailMatches(leg, tail) || !rotationTailMatches(cached, first(tail, leg.TailNumber)) ||
			!rotationAirportMatches(leg, next) || !rotationPrecedes(leg, next, cutoff, leg.ID == next.InboundID) {
			return prefix
		}
		seen[leg.ID] = true
		next, tail = leg, first(tail, leg.TailNumber, cached.TailNumber)
	}
	if next.InboundID == "" || next.InboundID != saved[len(prefix)-1].InboundID {
		return prefix
	}
	suffix := verifiedRotationSuffix(next, tail, cutoff, seen, saved[len(prefix):min(len(saved), maxRotationLegs)], true)
	if len(suffix) == 0 {
		return prefix
	}
	result := make([]Flight, 0, len(prefix)+len(suffix))
	result = append(result, prefix...)
	return append(result, suffix...)
}

// RevalidateRotationSuffix keeps the connected prefix of older legs when a
// newer observation replaces their direct leg. An unchanged explicit link
// can still have a verified tail fallback. A revised link must agree with the
// first older leg before that history can be carried forward.
func RevalidateRotationSuffix(flight, prior, direct Flight, suffix []Flight) []Flight {
	if len(suffix) == 0 {
		return suffix
	}
	departure := rotationDeparture(flight)
	if departure == nil || direct.InboundID != "" && direct.InboundID != prior.InboundID && direct.InboundID != suffix[0].ID {
		return suffix[:0]
	}
	cutoff := departure.Add(-rotationWindow)
	seen := map[string]bool{flight.ID: true, flight.AeroProviderID(): true, direct.ID: true}
	tail := first(flight.TailNumber, direct.TailNumber, prior.TailNumber)
	return verifiedRotationSuffix(direct, tail, cutoff, seen, suffix, false)
}

func verifiedRotationSuffix(next Flight, tail string, cutoff time.Time, seen map[string]bool, suffix []Flight, requireExplicitMatch bool) []Flight {
	for i, older := range suffix {
		linked := older.ID == next.InboundID
		if !providerIdentifier.MatchString(older.ID) || seen[older.ID] || older.ObservedAt.IsZero() ||
			requireExplicitMatch && next.InboundID != "" && !linked ||
			!linked && (tail == "" || older.TailNumber == "") || !rotationTailMatches(older, tail) ||
			!rotationAirportMatches(older, next) || !rotationPrecedes(older, next, cutoff, linked) {
			return suffix[:i]
		}
		seen[older.ID] = true
		next, tail = older, first(tail, older.TailNumber)
	}
	return suffix
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
	// A diversion can share its provider ID with an alternate-destination
	// row. Keep both as movement evidence, but exclude either from the chain.
	excluded := make(map[string]bool)
	diverted := make(map[string]bool)
	flights := make([]Flight, 0, len(rows))
	for _, row := range rows {
		if row.Blocked || row.Cancelled {
			excluded[row.ID] = true
			continue
		}
		if row.Diverted {
			diverted[row.ID] = true
		}
		if providerIdentifier.MatchString(row.ID) && strings.EqualFold(row.Registration, tail) {
			flight := a.normalizeFlight(row, now)
			switch flight.Status {
			case "cancelled":
				excluded[row.ID] = true
				continue
			case "diverted":
				diverted[row.ID] = true
			}
			flights = append(flights, flight)
		}
	}
	kept := flights[:0]
	for _, flight := range flights {
		if excluded[flight.ID] {
			continue
		}
		if diverted[flight.ID] {
			flight.Status = "diverted"
		}
		kept = append(kept, flight)
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
	if departed == nil || departed.Before(cutoff) || departed.After(inbound.ObservedAt) {
		return nil
	}
	// A diversion record may still name its originally planned destination.
	if inbound.Status != "diverted" &&
		(first(inbound.DestinationICAO, inbound.Destination) == "" || rotationAirportMatches(inbound, next)) {
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
	return rotationSameAirport(inbound.Destination, inbound.DestinationICAO, next.Origin, next.OriginICAO)
}

func rotationSameAirport(a, aICAO, b, bICAO string) bool {
	code := first(aICAO, a)
	if code == "" {
		return false
	}
	if code == first(bICAO, b) {
		return true
	}
	return (aICAO == "" || bICAO == "") && a != "" && a == b
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
