package feeds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

const (
	MaxSWIMPositionBytes  = 16 << 10
	SWIMPositionFreshness = 5 * time.Minute
)

// SWIMPositionReport binds operator-normalized airborne fixes to an exact
// provider instance. The bridge must already have filtered restricted data.
type SWIMPositionReport struct {
	ProviderFlightID   string
	OriginICAO         string
	DestinationICAO    string
	ScheduledDeparture time.Time
	ActualTakeoff      time.Time
	Positions          []Position
}

func DecodeSWIMPositionReport(input io.Reader, now time.Time) (SWIMPositionReport, error) {
	invalid := fmt.Errorf("%w: invalid SWIM position report", ErrInvalidQuery)
	body, err := io.ReadAll(io.LimitReader(input, MaxSWIMPositionBytes+1))
	if err != nil || len(body) > MaxSWIMPositionBytes || !utf8.Valid(body) {
		return SWIMPositionReport{}, invalid
	}
	var wire struct {
		Version            int       `json:"version"`
		DisplayAllowed     bool      `json:"displayAllowed"`
		ProviderFlightID   string    `json:"providerFlightId"`
		OriginICAO         string    `json:"originICAO"`
		DestinationICAO    string    `json:"destinationICAO"`
		ScheduledDeparture time.Time `json:"scheduledDeparture"`
		ActualTakeoff      time.Time `json:"actualTakeoff"`
		Positions          []struct {
			Latitude         *float64  `json:"latitude"`
			Longitude        *float64  `json:"longitude"`
			AltitudeFeet     *float64  `json:"altitudeFeet"`
			GroundSpeedKnots *float64  `json:"groundSpeedKnots"`
			VerticalRateFPM  *float64  `json:"verticalRateFpm"`
			Heading          *float64  `json:"heading"`
			At               time.Time `json:"at"`
		} `json:"positions"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF ||
		wire.Version != 1 || !wire.DisplayAllowed || !providerIdentifier.MatchString(wire.ProviderFlightID) ||
		!aeroAlertAirport.MatchString(wire.OriginICAO) || !aeroAlertAirport.MatchString(wire.DestinationICAO) ||
		wire.ScheduledDeparture.Year() < 2000 || wire.ScheduledDeparture.Year() > 2100 ||
		wire.ActualTakeoff.Year() < 2000 || wire.ActualTakeoff.After(now) ||
		len(wire.Positions) < 1 || len(wire.Positions) > 32 {
		return SWIMPositionReport{}, invalid
	}
	report := SWIMPositionReport{
		ProviderFlightID: wire.ProviderFlightID, OriginICAO: wire.OriginICAO, DestinationICAO: wire.DestinationICAO,
		ScheduledDeparture: wire.ScheduledDeparture.UTC(), ActualTakeoff: wire.ActualTakeoff.UTC(),
		Positions: make([]Position, 0, len(wire.Positions)),
	}
	var previous time.Time
	for _, p := range wire.Positions {
		if p.Latitude == nil || p.Longitude == nil || !swimPositionRange(p.Latitude, -90, 90) || !swimPositionRange(p.Longitude, -180, 180) ||
			!swimPositionRange(p.AltitudeFeet, -2000, 100000) || !swimPositionRange(p.GroundSpeedKnots, 0, 2500) ||
			!swimPositionRange(p.VerticalRateFPM, -25000, 25000) || !swimPositionRange(p.Heading, 0, 360) ||
			p.At.Before(report.ActualTakeoff) || p.At.Before(now.Add(-SWIMPositionFreshness)) || p.At.After(now) || !p.At.After(previous) {
			return SWIMPositionReport{}, invalid
		}
		report.Positions = append(report.Positions, Position{
			Latitude: *p.Latitude, Longitude: *p.Longitude, AltitudeFeet: p.AltitudeFeet,
			GroundSpeedKnots: p.GroundSpeedKnots, VerticalRateFPM: p.VerticalRateFPM, Heading: p.Heading,
			At: p.At.UTC(), Source: FAASWIM, Kind: "radar",
		})
		previous = p.At
	}
	return report, nil
}

func swimPositionRange(value *float64, minimum, maximum float64) bool {
	return value == nil || *value >= minimum && *value <= maximum
}

func (r SWIMPositionReport) MatchesFlight(f Flight) bool {
	return f.OriginICAO == r.OriginICAO && f.DestinationICAO == r.DestinationICAO &&
		f.ScheduledDeparture != nil && f.ScheduledDeparture.Equal(r.ScheduledDeparture) &&
		f.ActualTakeoff != nil && f.ActualTakeoff.Equal(r.ActualTakeoff) &&
		(f.Status == "airborne" || f.Status == "diverted") && f.ActualLanding == nil && f.ActualArrival == nil
}
