package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const swimPositionKeyFile = "PLANO_SWIM_POSITION_KEY_FILE"

func forwardSWIMPositionFromEnv(ctx context.Context, input io.Reader) (int, error) {
	key, err := readWebhookKeyFile(swimPositionKeyFile)
	if err != nil {
		return relayConfigError, err
	}
	return forwardSWIMPosition(ctx, input, os.Getenv("PLANO_SWIM_POSITION_RELAY_URL"), key, nil)
}

func forwardSWIMPosition(ctx context.Context, input io.Reader, endpoint string, key []byte, transport http.RoundTripper) (int, error) {
	if !validWebhookRelayEndpoint(endpoint, "/v1/webhooks/swim-position") {
		return relayConfigError, errors.New("PLANO_SWIM_POSITION_RELAY_URL must be an HTTPS or loopback HTTP URL ending in /v1/webhooks/swim-position")
	}
	if len(key) < 32 {
		return relayConfigError, errors.New("PLANO_SWIM_POSITION_KEY_FILE requires at least 32 bytes")
	}
	raw, err := io.ReadAll(io.LimitReader(input, feeds.MaxSWIMPositionBytes+1))
	if err != nil {
		return relayTempFailure, errors.New("could not read SWIM position report from standard input")
	}
	if _, err := feeds.DecodeSWIMPositionReport(bytes.NewReader(raw), time.Now().UTC()); err != nil {
		return relayDataError, errors.New("standard input did not yield a valid SWIM position report")
	}
	if transport == nil {
		direct := newWebhookRelayTransport()
		defer direct.CloseIdleConnections()
		transport = direct
	}
	return sendSignedWebhook(ctx, endpoint, "SWIM position", key, raw, transport)
}
