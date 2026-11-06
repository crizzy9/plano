package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const swimPositionKeyFile = "PLANO_SWIM_POSITION_KEY_FILE"
const eurocontrolPositionKeyFile = "PLANO_EUROCONTROL_POSITION_KEY_FILE"

type radarPositionRelay struct {
	source  string
	keyFile string
	urlEnv  string
	path    string
	command string
	mode    string
	convert func(io.Reader, string, time.Time) (io.Reader, int, error)
}

var (
	swimPositionRelay = radarPositionRelay{
		source: feeds.FAASWIM, keyFile: swimPositionKeyFile, urlEnv: "PLANO_SWIM_POSITION_RELAY_URL", path: "/v1/webhooks/swim-position",
		command: "forward-swim-position", mode: "sfdps-track", convert: sfdpsPositionRelayInput,
	}
	eurocontrolPositionRelay = radarPositionRelay{
		source: feeds.Eurocontrol, keyFile: eurocontrolPositionKeyFile, urlEnv: "PLANO_EUROCONTROL_POSITION_RELAY_URL", path: "/v1/webhooks/eurocontrol-position",
		command: "forward-eurocontrol-position", mode: "cat062-track", convert: cat062PositionRelayInput,
	}
)

func (relay radarPositionRelay) fromEnv(ctx context.Context, input io.Reader, args []string) (int, error) {
	if len(args) != 0 {
		if len(args) != 2 || args[0] != relay.mode {
			return relayConfigError, fmt.Errorf("usage: plano %s [%s <binding-file>]", relay.command, relay.mode)
		}
		converted, status, err := relay.convert(input, args[1], time.Now().UTC())
		if converted == nil || err != nil {
			return status, err
		}
		input = converted
	}
	key, err := readWebhookKeyFile(relay.keyFile)
	if err != nil {
		return relayConfigError, err
	}
	return relay.forward(ctx, input, os.Getenv(relay.urlEnv), key, nil)
}

func encodeRadarPositionRelay(report feeds.RadarPositionReport) (io.Reader, int, error) {
	type fix struct {
		At               time.Time `json:"at"`
		Latitude         float64   `json:"latitude"`
		Longitude        float64   `json:"longitude"`
		AltitudeFeet     *float64  `json:"altitudeFeet,omitempty"`
		GroundSpeedKnots *float64  `json:"groundSpeedKnots,omitempty"`
		VerticalRateFPM  *float64  `json:"verticalRateFpm,omitempty"`
	}
	wire := struct {
		Version            int       `json:"version"`
		DisplayAllowed     bool      `json:"displayAllowed"`
		ObservedAirborne   bool      `json:"observedAirborne"`
		ProviderFlightID   string    `json:"providerFlightId"`
		OriginICAO         string    `json:"originICAO"`
		DestinationICAO    string    `json:"destinationICAO"`
		TailNumber         string    `json:"tailNumber,omitempty"`
		ScheduledDeparture time.Time `json:"scheduledDeparture"`
		ActualTakeoff      time.Time `json:"actualTakeoff"`
		Positions          []fix     `json:"positions"`
	}{
		Version: 1, DisplayAllowed: true, ObservedAirborne: true,
		ProviderFlightID: report.ProviderFlightID, OriginICAO: report.OriginICAO, DestinationICAO: report.DestinationICAO,
		TailNumber:         report.TailNumber,
		ScheduledDeparture: report.ScheduledDeparture, ActualTakeoff: report.ActualTakeoff,
		Positions: make([]fix, 0, len(report.Positions)),
	}
	for _, p := range report.Positions {
		wire.Positions = append(wire.Positions, fix{
			At: p.At, Latitude: p.Latitude, Longitude: p.Longitude,
			AltitudeFeet: p.AltitudeFeet, GroundSpeedKnots: p.GroundSpeedKnots, VerticalRateFPM: p.VerticalRateFPM,
		})
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, relayDataError, errors.New("could not encode the radar position report")
	}
	return bytes.NewReader(body), 0, nil
}

func (relay radarPositionRelay) forward(ctx context.Context, input io.Reader, endpoint string, key []byte, transport http.RoundTripper) (int, error) {
	if !validWebhookRelayEndpoint(endpoint, relay.path) {
		return relayConfigError, fmt.Errorf("%s must be an HTTPS or loopback HTTP URL ending in %s", relay.urlEnv, relay.path)
	}
	if len(key) < 32 {
		return relayConfigError, fmt.Errorf("%s requires at least 32 bytes", relay.keyFile)
	}
	raw, err := io.ReadAll(io.LimitReader(input, feeds.MaxRadarPositionBytes+1))
	if err != nil {
		return relayTempFailure, errors.New("could not read radar position report from standard input")
	}
	if _, err := feeds.DecodeRadarPositionReport(bytes.NewReader(raw), time.Now().UTC(), relay.source); err != nil {
		return relayDataError, errors.New("standard input did not yield a valid radar position report")
	}
	if transport == nil {
		direct := newWebhookRelayTransport()
		defer direct.CloseIdleConnections()
		transport = direct
	}
	return sendSignedWebhook(ctx, endpoint, "radar position", key, raw, transport)
}
