package feeds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxATFMSlotBytes = 4 << 10
	ATFMAssigned     = "assigned"
	ATFMWithdrawn    = "withdrawn"
	atfmSlotMaxAge   = 30 * time.Minute
)

var errInvalidATFMSlot = fmt.Errorf("%w: invalid ATFM slot", ErrInvalidQuery)

// ATFMSlot is an authorized bridge's report of a flight-specific runway time.
// ScheduledDeparture identifies the flight instance's airline schedule.
type ATFMSlot struct {
	Version            int        `json:"version"`
	Source             string     `json:"source"`
	ProviderFlightID   string     `json:"providerFlightId"`
	OriginICAO         string     `json:"originIcao"`
	DestinationICAO    string     `json:"destinationIcao"`
	ScheduledDeparture time.Time  `json:"scheduledDeparture"`
	ObservedAt         time.Time  `json:"observedAt"`
	Status             string     `json:"status"`
	TakeoffAt          *time.Time `json:"takeoffAt,omitempty"`
	Reason             string     `json:"reason,omitempty"`
}

func DecodeATFMSlot(input io.Reader, now time.Time) (ATFMSlot, error) {
	body, err := io.ReadAll(io.LimitReader(input, MaxATFMSlotBytes+1))
	if err != nil || len(body) > MaxATFMSlotBytes || !utf8.Valid(body) {
		return ATFMSlot{}, errInvalidATFMSlot
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var slot ATFMSlot
	if decoder.Decode(&slot) != nil || decoder.Decode(new(any)) != io.EOF {
		return ATFMSlot{}, errInvalidATFMSlot
	}
	return normalizeATFMSlot(slot, now)
}

func normalizeATFMSlot(slot ATFMSlot, now time.Time) (ATFMSlot, error) {
	if slot.Version != 1 || slot.Source != FAASWIM && slot.Source != Eurocontrol || !providerIdentifier.MatchString(slot.ProviderFlightID) ||
		!aeroAlertAirport.MatchString(slot.OriginICAO) || !aeroAlertAirport.MatchString(slot.DestinationICAO) ||
		slot.ObservedAt.Before(now.Add(-atfmSlotMaxAge)) || slot.ObservedAt.After(now) ||
		slot.ScheduledDeparture.Before(now.Add(-12*time.Hour)) || slot.ScheduledDeparture.After(now.Add(24*time.Hour)) {
		return ATFMSlot{}, errInvalidATFMSlot
	}
	switch slot.Status {
	case ATFMAssigned:
		if slot.TakeoffAt == nil || slot.TakeoffAt.Before(slot.ObservedAt.Add(-10*time.Minute)) ||
			slot.TakeoffAt.After(slot.ObservedAt.Add(24*time.Hour)) {
			return ATFMSlot{}, errInvalidATFMSlot
		}
		takeoff := slot.TakeoffAt.UTC()
		slot.TakeoffAt = &takeoff
	case ATFMWithdrawn:
		if slot.TakeoffAt != nil {
			return ATFMSlot{}, errInvalidATFMSlot
		}
	default:
		return ATFMSlot{}, errInvalidATFMSlot
	}
	if len(slot.Reason) > 160 || strings.ContainsFunc(slot.Reason, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' || r == unicode.ReplacementChar
	}) {
		return ATFMSlot{}, errInvalidATFMSlot
	}
	slot.Reason = strings.TrimSpace(slot.Reason)
	slot.ObservedAt, slot.ScheduledDeparture = slot.ObservedAt.UTC(), slot.ScheduledDeparture.UTC()
	return slot, nil
}

// ValidUntil bounds presentation by both report freshness and the end of the
// assigned takeoff window. The timestamp remains stored for revision ordering.
func (slot ATFMSlot) ValidUntil() time.Time {
	until := slot.ObservedAt.Add(atfmSlotMaxAge)
	if slot.TakeoffAt != nil {
		late := 5 * time.Minute
		if slot.Source == Eurocontrol {
			late = 10 * time.Minute
		}
		if windowEnd := slot.TakeoffAt.Add(late); windowEnd.Before(until) {
			return windowEnd
		}
	}
	return until
}

// MatchesFlight checks route, schedule and phase. Callers verify the provider
// instance independently against the authoritative AeroAPI alias table.
func (slot ATFMSlot) MatchesFlight(f Flight) bool {
	if f.OriginICAO != slot.OriginICAO || f.DestinationICAO != slot.DestinationICAO ||
		f.ScheduledDeparture == nil || !f.ScheduledDeparture.Equal(slot.ScheduledDeparture) ||
		f.ActualTakeoff != nil || f.ActualLanding != nil || f.ActualArrival != nil {
		return false
	}
	return f.Status == "scheduled" || f.Status == "boarding" || f.Status == "taxi_out"
}
