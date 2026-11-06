package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const flightObservationKeyFile = "PLANO_FLIGHT_OBSERVATION_KEY_FILE"

func readWebhookKeyFile(variable string) ([]byte, error) {
	path := os.Getenv(variable)
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must name a readable key file", variable)
	}
	key, err := secret("", variable)
	if err != nil {
		return nil, err
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("%s requires at least 32 bytes", variable)
	}
	return key, nil
}

func forwardFlightObservationFromEnv(ctx context.Context, input io.Reader) (int, error) {
	key, err := readWebhookKeyFile(flightObservationKeyFile)
	if err != nil {
		return relayConfigError, err
	}
	return forwardFlightObservation(ctx, input, os.Getenv("PLANO_FLIGHT_OBSERVATION_RELAY_URL"), key, nil)
}

func forwardFlightObservation(ctx context.Context, input io.Reader, endpoint string, key []byte, transport http.RoundTripper) (int, error) {
	if !validWebhookRelayEndpoint(endpoint, "/v1/webhooks/flight-observation") {
		return relayConfigError, errors.New("PLANO_FLIGHT_OBSERVATION_RELAY_URL must be an HTTPS or loopback HTTP URL ending in /v1/webhooks/flight-observation")
	}
	if len(key) < 32 {
		return relayConfigError, errors.New("PLANO_FLIGHT_OBSERVATION_KEY_FILE requires at least 32 bytes")
	}
	raw, err := io.ReadAll(io.LimitReader(input, feeds.MaxFlightObservationBytes+1))
	if err != nil {
		return relayTempFailure, errors.New("could not read flight observation from standard input")
	}
	if len(raw) > feeds.MaxFlightObservationBytes {
		return relayDataError, errors.New("flight observation exceeds the relay size limit")
	}
	if _, err := feeds.DecodeFlightObservation(bytes.NewReader(raw), time.Now().UTC()); err != nil {
		if errors.Is(err, feeds.ErrNotFound) {
			return 0, nil
		}
		return relayDataError, errors.New("standard input did not yield a valid flight observation envelope")
	}
	if transport == nil {
		direct := newWebhookRelayTransport()
		defer direct.CloseIdleConnections()
		transport = direct
	}
	return sendSignedWebhook(ctx, endpoint, "flight observation", key, raw, transport)
}
