package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func runATFMSlotRelay(ctx context.Context, input io.Reader, args []string) (int, error) {
	input, status, err := atfmSlotRelayInput(input, args, time.Now().UTC())
	if err != nil || input == nil {
		return status, err
	}
	key, err := readAdvisoryRelayKey()
	if err != nil {
		return relayConfigError, err
	}
	return forwardATFMSlot(ctx, input, os.Getenv("PLANO_ATFM_SLOT_RELAY_URL"), key, nil)
}

func atfmSlotRelayInput(input io.Reader, args []string, now time.Time) (io.Reader, int, error) {
	if len(args) == 0 {
		return input, 0, nil
	}
	if len(args) == 2 && args[0] == "tfms-flight-control" {
		return tfmsATFMSlotRelayInput(input, args[1], now)
	}
	if (len(args) != 5 && len(args) != 6) || args[0] != "eurocontrol-adexp" {
		return nil, relayConfigError, errors.New("usage: plano forward-atfm-slot [tfms-flight-control <binding-file> | eurocontrol-adexp <fa-flight-id> <ifplid> <scheduled-departure> <observed-at|-> [takeoff-date]]")
	}
	scheduled, err := time.Parse(time.RFC3339Nano, args[3])
	if err != nil || scheduled.IsZero() {
		return nil, relayConfigError, errors.New("scheduled-departure must be an RFC 3339 instant")
	}
	binding := feeds.ADEXPATFMSlotBinding{ProviderFlightID: args[1], IFPLID: args[2], ScheduledDeparture: scheduled}
	if args[4] != "-" {
		binding.ObservedAt, err = time.Parse(time.RFC3339Nano, args[4])
		if err != nil || binding.ObservedAt.IsZero() {
			return nil, relayConfigError, errors.New("observed-at must be an RFC 3339 instant or '-' for a source timestamp")
		}
	}
	if len(args) == 6 {
		if _, err := time.Parse(time.DateOnly, args[5]); err != nil {
			return nil, relayConfigError, errors.New("takeoff-date must be a UTC date in YYYY-MM-DD format")
		}
		binding.TakeoffDate = args[5]
	}
	slot, err := feeds.DecodeADEXPATFMSlot(input, binding, now)
	if errors.Is(err, feeds.ErrUnavailable) {
		return nil, relayTempFailure, errors.New("could not read EUROCONTROL ADEXP from standard input")
	}
	if err != nil {
		return nil, relayDataError, errors.New("standard input did not yield a valid, bound EUROCONTROL slot message")
	}
	body, err := json.Marshal(slot)
	if err != nil {
		return nil, relayDataError, errors.New("could not encode the EUROCONTROL slot")
	}
	return bytes.NewReader(body), 0, nil
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
