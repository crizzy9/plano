package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/itinerary"
)

// Webhook relays use sysexits to distinguish permanent failures from retries.
const (
	relayDataError   = 65
	relayTempFailure = 75
	relayPermission  = 77
	relayConfigError = 78
)

func forwardEmailFromEnv(ctx context.Context, input io.Reader) (int, error) {
	info, err := os.Stat(os.Getenv("PLANO_EMAIL_RELAY_TOKEN_FILE"))
	if err != nil || !info.Mode().IsRegular() {
		return relayConfigError, errors.New("PLANO_EMAIL_RELAY_TOKEN_FILE must name a readable token file")
	}
	token, err := secret("", "PLANO_EMAIL_RELAY_TOKEN_FILE")
	if err != nil {
		return relayConfigError, err
	}
	return forwardEmail(ctx, input, os.Getenv("PLANO_EMAIL_RELAY_URL"), string(token), nil)
}

func forwardEmail(ctx context.Context, input io.Reader, endpoint, token string, transport http.RoundTripper) (int, error) {
	if !validEmailRelayEndpoint(endpoint) {
		return relayConfigError, errors.New("PLANO_EMAIL_RELAY_URL must be an HTTPS or loopback HTTP URL ending in /v1/webhooks/email")
	}
	if len(token) < 20 || len(token) > 128 || strings.ContainsFunc(token, func(r rune) bool { return r < '!' || r > '~' }) {
		return relayConfigError, errors.New("PLANO_EMAIL_RELAY_TOKEN_FILE must contain a Plano email relay token")
	}
	raw, err := io.ReadAll(io.LimitReader(input, itinerary.MaxEmailBytes+1))
	if err != nil {
		return relayTempFailure, errors.New("could not read email from standard input")
	}
	if len(raw) > itinerary.MaxEmailBytes {
		return relayDataError, errors.New("email exceeds the relay size limit")
	}
	if _, err := mail.ReadMessage(bytes.NewReader(raw)); err != nil {
		return relayDataError, errors.New("standard input is not a readable RFC 5322 message")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return relayConfigError, errors.New("could not construct email relay request")
	}
	request.Header.Set("Content-Type", "message/rfc822")
	request.Header.Set("Authorization", "Bearer "+token)
	if transport == nil {
		direct := newWebhookRelayTransport()
		defer direct.CloseIdleConnections()
		transport = direct
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		return relayTempFailure, errors.New("could not reach the email relay endpoint")
	}
	defer response.Body.Close()
	return webhookRelayStatus(response.StatusCode, "email")
}

func newWebhookRelayTransport() *http.Transport {
	direct := http.DefaultTransport.(*http.Transport).Clone()
	direct.Proxy = nil
	direct.MaxResponseHeaderBytes = 32 << 10
	return direct
}

func webhookRelayStatus(status int, kind string) (int, error) {
	if status == http.StatusNoContent {
		return 0, nil
	}
	// Keep provider content out of relay logs and delivery status notices.
	result := fmt.Errorf("%s relay returned HTTP %d", kind, status)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return relayPermission, result
	case http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return relayDataError, result
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return relayTempFailure, result
	default:
		if status >= 500 && status < 600 {
			return relayTempFailure, result
		}
		return relayConfigError, result
	}
}

func validEmailRelayEndpoint(raw string) bool {
	return validWebhookRelayEndpoint(raw, "/v1/webhooks/email")
}

func validWebhookRelayEndpoint(raw, path string) bool {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.EscapedPath() != path ||
		endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(raw, "#") || strings.HasSuffix(endpoint.Host, ":") {
		return false
	}
	if port := endpoint.Port(); port != "" {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return false
		}
	}
	return endpoint.Scheme == "https" || endpoint.Scheme == "http" && net.ParseIP(endpoint.Hostname()).IsLoopback()
}
