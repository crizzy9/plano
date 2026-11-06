package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func sfdpsPositionRelayInput(input io.Reader, bindingPath string, now time.Time) (io.Reader, int, error) {
	invalid := errors.New("FAA SFDPS binding must be a readable version 1 JSON file of at most 4 KiB with displayAllowed enabled")
	var binding feeds.SFDPSPositionBinding
	if err := readRelayBinding(bindingPath, &binding, invalid); err != nil || binding.Version != 1 || !binding.DisplayAllowed {
		return nil, relayConfigError, invalid
	}
	report, err := feeds.DecodeSFDPSPositions(input, binding, now)
	switch {
	case errors.Is(err, feeds.ErrNotFound):
		return nil, 0, nil
	case errors.Is(err, feeds.ErrUnavailable):
		return nil, relayTempFailure, errors.New("could not read FAA SFDPS XML from standard input")
	case err != nil:
		return nil, relayDataError, errors.New("standard input did not yield a valid, bound FAA SFDPS position report")
	}
	type fix struct {
		At               time.Time `json:"at"`
		Latitude         float64   `json:"latitude"`
		Longitude        float64   `json:"longitude"`
		AltitudeFeet     *float64  `json:"altitudeFeet,omitempty"`
		GroundSpeedKnots *float64  `json:"groundSpeedKnots,omitempty"`
	}
	wire := struct {
		Version            int       `json:"version"`
		DisplayAllowed     bool      `json:"displayAllowed"`
		ObservedAirborne   bool      `json:"observedAirborne"`
		ProviderFlightID   string    `json:"providerFlightId"`
		OriginICAO         string    `json:"originICAO"`
		DestinationICAO    string    `json:"destinationICAO"`
		ScheduledDeparture time.Time `json:"scheduledDeparture"`
		ActualTakeoff      time.Time `json:"actualTakeoff"`
		Positions          []fix     `json:"positions"`
	}{
		Version: 1, DisplayAllowed: true, ObservedAirborne: true,
		ProviderFlightID: report.ProviderFlightID, OriginICAO: report.OriginICAO, DestinationICAO: report.DestinationICAO,
		ScheduledDeparture: report.ScheduledDeparture, ActualTakeoff: report.ActualTakeoff,
		Positions: make([]fix, 0, len(report.Positions)),
	}
	for _, p := range report.Positions {
		wire.Positions = append(wire.Positions, fix{
			At: p.At, Latitude: p.Latitude, Longitude: p.Longitude, AltitudeFeet: p.AltitudeFeet, GroundSpeedKnots: p.GroundSpeedKnots,
		})
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, relayDataError, errors.New("could not encode the FAA SFDPS position report")
	}
	return bytes.NewReader(body), 0, nil
}
