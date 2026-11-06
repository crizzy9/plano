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

// MTA pipe delivery uses sysexits to distinguish permanent failures from retries.
const (
	emailRelayDataError   = 65
	emailRelayTempFailure = 75
	emailRelayPermission  = 77
	emailRelayConfigError = 78
)

func forwardEmailFromEnv(ctx context.Context, input io.Reader) (int, error) {
	info, err := os.Stat(os.Getenv("PLANO_EMAIL_RELAY_TOKEN_FILE"))
	if err != nil || !info.Mode().IsRegular() {
		return emailRelayConfigError, errors.New("PLANO_EMAIL_RELAY_TOKEN_FILE must name a readable token file")
	}
	token, err := secret("", "PLANO_EMAIL_RELAY_TOKEN_FILE")
	if err != nil {
		return emailRelayConfigError, err
	}
	return forwardEmail(ctx, input, os.Getenv("PLANO_EMAIL_RELAY_URL"), string(token), nil)
}

func forwardEmail(ctx context.Context, input io.Reader, endpoint, token string, transport http.RoundTripper) (int, error) {
	if !validEmailRelayEndpoint(endpoint) {
		return emailRelayConfigError, errors.New("PLANO_EMAIL_RELAY_URL must be an HTTPS or loopback HTTP URL ending in /v1/webhooks/email")
	}
	if len(token) < 20 || len(token) > 128 || strings.ContainsFunc(token, func(r rune) bool { return r < '!' || r > '~' }) {
		return emailRelayConfigError, errors.New("PLANO_EMAIL_RELAY_TOKEN_FILE must contain a Plano email relay token")
	}
	raw, err := io.ReadAll(io.LimitReader(input, itinerary.MaxEmailBytes+1))
	if err != nil {
		return emailRelayTempFailure, errors.New("could not read email from standard input")
	}
	if len(raw) > itinerary.MaxEmailBytes {
		return emailRelayDataError, errors.New("email exceeds the relay size limit")
	}
	if _, err := mail.ReadMessage(bytes.NewReader(raw)); err != nil {
		return emailRelayDataError, errors.New("standard input is not a readable RFC 5322 message")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return emailRelayConfigError, errors.New("could not construct email relay request")
	}
	request.Header.Set("Content-Type", "message/rfc822")
	request.Header.Set("Authorization", "Bearer "+token)
	if transport == nil {
		direct := http.DefaultTransport.(*http.Transport).Clone()
		direct.Proxy = nil
		direct.MaxResponseHeaderBytes = 32 << 10
		defer direct.CloseIdleConnections()
		transport = direct
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		return emailRelayTempFailure, errors.New("could not reach the email relay endpoint")
	}
	defer response.Body.Close()
	status := response.StatusCode
	if status == http.StatusNoContent {
		return 0, nil
	}
	// Keep provider content out of MTA logs and delivery status notices.
	result := fmt.Errorf("email relay returned HTTP %d", status)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return emailRelayPermission, result
	case http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return emailRelayDataError, result
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return emailRelayTempFailure, result
	default:
		if status >= 500 && status < 600 {
			return emailRelayTempFailure, result
		}
		return emailRelayConfigError, result
	}
}

func validEmailRelayEndpoint(raw string) bool {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.EscapedPath() != "/v1/webhooks/email" ||
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
