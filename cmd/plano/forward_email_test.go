package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/itinerary"
)

const (
	relayFixtureURL   = "https://relay.invalid/v1/webhooks/email"
	relayFixtureToken = "fixture-email-relay-token"
	relayFixtureMail  = "Subject: UA123 Jan 5 2027\r\nContent-Type: text/plain\r\n\r\nFlight UA123\r\n"
)

type relayTransport func(*http.Request) (*http.Response, error)

func (f relayTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type relayResponseBody struct{ read, closed bool }

func (b *relayResponseBody) Read([]byte) (int, error) {
	b.read = true
	return 0, io.EOF
}

func (b *relayResponseBody) Close() error {
	b.closed = true
	return nil
}

func TestForwardEmailPreservesMessageAndClassifiesResponse(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status, exit int
	}{
		{"accepted", http.StatusNoContent, 0},
		{"partial import", http.StatusServiceUnavailable, 75},
		{"upstream error", http.StatusBadGateway, 75},
		{"timeout", http.StatusRequestTimeout, 75},
		{"throttled", http.StatusTooManyRequests, 75},
		{"oversize", http.StatusRequestEntityTooLarge, 65},
		{"unsupported", http.StatusUnsupportedMediaType, 65},
		{"unmatched", http.StatusUnprocessableEntity, 65},
		{"unauthorized", http.StatusUnauthorized, 77},
		{"forbidden", http.StatusForbidden, 77},
		{"wrong endpoint", http.StatusOK, 78},
		{"redirect", http.StatusTemporaryRedirect, 78},
		{"redirect with malformed location", http.StatusFound, 78},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests int
			responseBody := &relayResponseBody{}
			transport := relayTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				defer r.Body.Close()
				raw, err := io.ReadAll(r.Body)
				if err != nil || string(raw) != relayFixtureMail || r.ContentLength != int64(len(relayFixtureMail)) {
					t.Fatalf("relay request did not preserve message bytes and length")
				}
				if r.Method != http.MethodPost || r.URL.String() != relayFixtureURL ||
					r.Header.Get("Content-Type") != "message/rfc822" || r.Header.Get("Authorization") != "Bearer "+relayFixtureToken {
					t.Fatal("relay request metadata did not match the configured endpoint")
				}
				location := "https://redirect.invalid/elsewhere"
				if tc.status == http.StatusFound {
					location = "http://[::1"
				}
				return &http.Response{
					StatusCode: tc.status,
					Header:     http.Header{"Location": {location}},
					Body:       responseBody,
				}, nil
			})
			got, err := forwardEmail(t.Context(), strings.NewReader(relayFixtureMail), relayFixtureURL, relayFixtureToken, transport)
			if got != tc.exit || (err == nil) != (tc.exit == 0) || requests != 1 || responseBody.read || !responseBody.closed {
				t.Fatalf("result = %d, %v, requests = %d, response read = %t, closed = %t", got, err, requests, responseBody.read, responseBody.closed)
			}
		})
	}
}

func TestForwardEmailRedactsTransportErrors(t *testing.T) {
	transport := relayTransport(func(r *http.Request) (*http.Response, error) {
		r.Body.Close()
		return nil, errors.New("sensitive fixture content " + relayFixtureToken)
	})
	status, err := forwardEmail(t.Context(), strings.NewReader(relayFixtureMail), relayFixtureURL, relayFixtureToken, transport)
	if status != 75 || err == nil || err.Error() != "could not reach the email relay endpoint" {
		t.Fatalf("expected a redacted temporary failure, got exit %d", status)
	}
}

func TestForwardEmailChecksMessageBeforeSending(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      int
	}{
		{"empty", "", 65},
		{"malformed", "this is not a header\n\nbody", 65},
		{"at limit", relayFixtureMail + strings.Repeat("x", itinerary.MaxEmailBytes-len(relayFixtureMail)), 0},
		{"above limit", relayFixtureMail + strings.Repeat("x", itinerary.MaxEmailBytes-len(relayFixtureMail)+1), 65},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent bool
			transport := relayTransport(func(r *http.Request) (*http.Response, error) {
				r.Body.Close()
				sent = true
				return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
			})
			status, err := forwardEmail(t.Context(), strings.NewReader(tc.raw), relayFixtureURL, relayFixtureToken, transport)
			if status != tc.want || sent != (tc.want == 0) || (err == nil) != (tc.want == 0) {
				t.Fatalf("result = %d, %v, sent = %t", status, err, sent)
			}
		})
	}
}

func TestForwardEmailValidatesConfiguration(t *testing.T) {
	for _, endpoint := range []string{
		relayFixtureURL, "http://127.0.0.1:8080/v1/webhooks/email", "http://[::1]/v1/webhooks/email",
	} {
		if !validEmailRelayEndpoint(endpoint) {
			t.Errorf("rejected valid endpoint %q", endpoint)
		}
	}
	for _, endpoint := range []string{
		"", "/v1/webhooks/email", "http://relay.invalid/v1/webhooks/email", "http://localhost/v1/webhooks/email",
		"http://192.168.0.2/v1/webhooks/email", relayFixtureURL + "?", relayFixtureURL + "#", relayFixtureURL + "/",
		"https://user:pass@relay.invalid/v1/webhooks/email", "https://relay.invalid:99999/v1/webhooks/email",
		"https://relay.invalid/v1/webhooks/%65mail", "https://relay.invalid/v1/webhooks/../webhooks/email",
	} {
		if validEmailRelayEndpoint(endpoint) {
			t.Errorf("accepted invalid endpoint %q", endpoint)
		}
	}
	transport := relayTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid configuration reached the transport")
		return nil, nil
	})
	status, err := forwardEmail(t.Context(), strings.NewReader(relayFixtureMail), relayFixtureURL, relayFixtureToken+"\r\nX-Bad: yes", transport)
	if status != 78 || err == nil || strings.Contains(err.Error(), relayFixtureToken) {
		t.Fatalf("expected a redacted configuration error, got exit %d", status)
	}
}

func TestForwardEmailUsesFileCredential(t *testing.T) {
	t.Setenv("PLANO_EMAIL_RELAY_URL", relayFixtureURL)
	t.Setenv("PLANO_EMAIL_RELAY_TOKEN_FILE", "")
	t.Setenv("PLANO_EMAIL_RELAY_TOKEN", relayFixtureToken)
	if status, _ := forwardEmailFromEnv(t.Context(), strings.NewReader("")); status != 78 {
		t.Fatalf("command accepted missing token file, exit %d", status)
	}
	dir := t.TempDir()
	t.Setenv("PLANO_EMAIL_RELAY_TOKEN_FILE", dir)
	if status, _ := forwardEmailFromEnv(t.Context(), strings.NewReader("")); status != 78 {
		t.Fatalf("command accepted a token directory, exit %d", status)
	}
	path := filepath.Join(dir, "fixture-token")
	if err := os.WriteFile(path, []byte(relayFixtureToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLANO_EMAIL_RELAY_TOKEN_FILE", path)
	if status, _ := forwardEmailFromEnv(t.Context(), strings.NewReader("")); status != 65 {
		t.Fatalf("command did not reach input validation with a file credential, exit %d", status)
	}
}
