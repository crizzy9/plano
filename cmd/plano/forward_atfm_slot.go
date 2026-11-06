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

func forwardATFMSlotFromEnv(ctx context.Context, input io.Reader) (int, error) {
	key, err := readAdvisoryRelayKey()
	if err != nil {
		return relayConfigError, err
	}
	return forwardATFMSlot(ctx, input, os.Getenv("PLANO_ATFM_SLOT_RELAY_URL"), key, nil)
}

func forwardATFMSlot(ctx context.Context, input io.Reader, endpoint string, key []byte, transport http.RoundTripper) (int, error) {
	if !validWebhookRelayEndpoint(endpoint, "/v1/webhooks/atfm-slot") {
		return relayConfigError, errors.New("PLANO_ATFM_SLOT_RELAY_URL must be an HTTPS or loopback HTTP URL ending in /v1/webhooks/atfm-slot")
	}
	if len(key) < 32 {
		return relayConfigError, errors.New("PLANO_ADVISORY_INGRESS_KEY_FILE requires at least 32 bytes")
	}
	raw, err := io.ReadAll(io.LimitReader(input, feeds.MaxATFMSlotBytes+1))
	if err != nil {
		return relayTempFailure, errors.New("could not read ATFM slot from standard input")
	}
	if _, err := feeds.DecodeATFMSlot(bytes.NewReader(raw), time.Now().UTC()); err != nil {
		return relayDataError, errors.New("standard input did not yield a valid ATFM slot envelope")
	}
	if transport == nil {
		direct := newWebhookRelayTransport()
		defer direct.CloseIdleConnections()
		transport = direct
	}
	return sendSignedWebhook(ctx, endpoint, "ATFM slot", key, raw, transport)
}
