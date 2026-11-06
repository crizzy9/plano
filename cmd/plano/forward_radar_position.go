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

const swimPositionKeyFile = "PLANO_SWIM_POSITION_KEY_FILE"
const eurocontrolPositionKeyFile = "PLANO_EUROCONTROL_POSITION_KEY_FILE"

type radarPositionRelay struct {
	source  string
	keyFile string
	urlEnv  string
	path    string
}

var (
	swimPositionRelay        = radarPositionRelay{feeds.FAASWIM, swimPositionKeyFile, "PLANO_SWIM_POSITION_RELAY_URL", "/v1/webhooks/swim-position"}
	eurocontrolPositionRelay = radarPositionRelay{feeds.Eurocontrol, eurocontrolPositionKeyFile, "PLANO_EUROCONTROL_POSITION_RELAY_URL", "/v1/webhooks/eurocontrol-position"}
)

func (relay radarPositionRelay) fromEnv(ctx context.Context, input io.Reader, args []string) (int, error) {
	if len(args) != 0 {
		if relay.source != feeds.FAASWIM {
			return relayConfigError, errors.New("usage: plano forward-eurocontrol-position")
		}
		if len(args) != 2 || args[0] != "sfdps-track" {
			return relayConfigError, errors.New("usage: plano forward-swim-position [sfdps-track <binding-file>]")
		}
		converted, status, err := sfdpsPositionRelayInput(input, args[1], time.Now().UTC())
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
