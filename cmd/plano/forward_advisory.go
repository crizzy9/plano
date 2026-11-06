package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const maxAdvisoryRelayNotices = 32

func forwardAdvisoryFromEnv(ctx context.Context, input io.Reader, convert func(io.Reader) ([][]byte, error)) (int, error) {
	const keyFile = "PLANO_ADVISORY_INGRESS_KEY_FILE"
	info, err := os.Stat(os.Getenv(keyFile))
	if err != nil || !info.Mode().IsRegular() {
		return relayConfigError, errors.New("PLANO_ADVISORY_INGRESS_KEY_FILE must name a readable key file")
	}
	key, err := secret("", keyFile)
	if err != nil {
		return relayConfigError, err
	}
	return forwardAdvisory(ctx, input, os.Getenv("PLANO_ADVISORY_RELAY_URL"), key, convert, nil)
}

func forwardAdvisory(ctx context.Context, input io.Reader, endpoint string, key []byte, convert func(io.Reader) ([][]byte, error), transport http.RoundTripper) (int, error) {
	if !validWebhookRelayEndpoint(endpoint, "/v1/webhooks/advisory") {
		return relayConfigError, errors.New("PLANO_ADVISORY_RELAY_URL must be an HTTPS or loopback HTTP URL ending in /v1/webhooks/advisory")
	}
	if len(key) < 32 {
		return relayConfigError, errors.New("PLANO_ADVISORY_INGRESS_KEY_FILE requires at least 32 bytes")
	}
	var bodies [][]byte
	var deferred bool
	if convert == nil {
		raw, err := io.ReadAll(io.LimitReader(input, feeds.MaxAdvisoryIngressBytes+1))
		if err != nil {
			return relayTempFailure, errors.New("could not read advisory from standard input")
		}
		bodies = [][]byte{raw}
	} else {
		var err error
		bodies, err = convert(input)
		deferred = errors.Is(err, feeds.ErrAdvisoryNotYetEffective)
		if errors.Is(err, feeds.ErrUnavailable) {
			return relayTempFailure, errors.New("could not read advisory from standard input")
		}
		if err != nil && !deferred {
			return relayDataError, errors.New("could not convert advisory from standard input")
		}
	}
	if deferred && len(bodies) == 0 {
		return relayTempFailure, errors.New("advisory is not yet effective")
	}
	if len(bodies) == 0 || len(bodies) > maxAdvisoryRelayNotices {
		return relayDataError, errors.New("invalid number of advisory envelopes")
	}
	now := time.Now().UTC()
	for _, body := range bodies {
		if len(body) > feeds.MaxAdvisoryIngressBytes {
			return relayDataError, errors.New("advisory exceeds the relay size limit")
		}
		advisory, err := feeds.DecodeAdvisoryIngress(bytes.NewReader(body))
		if err != nil {
			return relayDataError, errors.New("standard input did not yield a valid advisory envelope")
		}
		if advisory.ObservedAt.After(now.Add(30*time.Second)) || advisory.ObservedAt.Before(now.Add(-6*time.Hour)) {
			return relayDataError, errors.New("advisory observation is outside the ingress time window")
		}
	}
	if transport == nil {
		direct := newWebhookRelayTransport()
		defer direct.CloseIdleConnections()
		transport = direct
	}
	// A failed delivery leaves earlier accepted notices in place. Replaying the
	// same provider bytes keeps their original observation and revision identity.
	for _, body := range bodies {
		if status, err := sendAdvisory(ctx, endpoint, key, body, transport); err != nil {
			return status, err
		}
	}
	if deferred {
		return relayTempFailure, errors.New("some advisories are not yet effective")
	}
	return 0, nil
}

func sendAdvisory(ctx context.Context, endpoint string, key, raw []byte, transport http.RoundTripper) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(stamp + "."))
	mac.Write(raw)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return relayConfigError, errors.New("could not construct advisory relay request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Plano-Timestamp", stamp)
	request.Header.Set("X-Plano-Signature", hex.EncodeToString(mac.Sum(nil)))
	response, err := transport.RoundTrip(request)
	if err != nil {
		return relayTempFailure, errors.New("could not reach the advisory relay endpoint")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusBadRequest {
		return relayDataError, fmt.Errorf("advisory relay returned HTTP %d", response.StatusCode)
	}
	return webhookRelayStatus(response.StatusCode, "advisory")
}
