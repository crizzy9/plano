package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestHealthEndpointUsesConfiguredListener(t *testing.T) {
	for _, tc := range []struct{ addr, override, want string }{
		{"", "", "http://127.0.0.1:8080/healthz"},
		{"0.0.0.0:8123", "", "http://127.0.0.1:8123/healthz"},
		{"[::]:8123", "", "http://[::1]:8123/healthz"},
		{"127.0.0.2:8123", "", "http://127.0.0.2:8123/healthz"},
		{"127.0.0.2:8123", "https://plano.invalid/healthz", "https://plano.invalid/healthz"},
		{"127.0.0.2:8123", "http://[::1]:8123/healthz", "http://[::1]:8123/healthz"},
		{"", "http://127.0.0.1/healthz?q=has%20space", "http://127.0.0.1/healthz?q=has%20space"},
	} {
		t.Setenv("PLANO_ADDR", tc.addr)
		t.Setenv("PLANO_HEALTH_URL", tc.override)
		got, err := healthEndpoint()
		if err != nil || got != tc.want {
			t.Fatalf("endpoint for %q = %q, %v, want %q", tc.addr, got, err, tc.want)
		}
	}
}

func TestHealthEndpointRejectsInvalidOverridesWithoutEchoingValues(t *testing.T) {
	for _, override := range []string{
		"ftp://127.0.0.1/healthz", "/healthz", "http:///healthz", "http://127.0.0.1:/healthz",
		"http://127.0.0.1:0/healthz", "https://example.invalid:65536/healthz",
		"http://::1:8123/healthz", "http://[fixture.invalid]:8123/healthz", "http://127.0.0.1]/healthz",
		"https://fixture-credential@example.invalid/healthz", "https://example.invalid/healthz#",
		"http://127.0.0.1/healthz?q=has space",
	} {
		t.Setenv("PLANO_HEALTH_URL", override)
		if endpoint, err := healthEndpoint(); endpoint != "" || err == nil ||
			!strings.Contains(err.Error(), "PLANO_HEALTH_URL") || strings.Contains(err.Error(), override) {
			t.Fatal("invalid override did not produce a sanitized configuration error")
		}
	}
}

type healthTransport func(*http.Request) (*http.Response, error)

func (f healthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckHealthValidatesPlanoResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantOK     bool
	}{
		{"demo", `{"status":"ok","mode":"demo"}`, 200, true},
		{"live", `{"status":"ok","mode":"live"}`, 200, true},
		{"reverse proxy page", `<html>OK</html>`, 200, false},
		{"unsupported mode", `{"status":"ok","mode":"other"}`, 200, false},
		{"trailing data", `{"status":"ok","mode":"demo"} {}`, 200, false},
		{"oversized", strings.Repeat(" ", 1025) + `{"status":"ok","mode":"demo"}`, 200, false},
		{"redirect", `{"status":"ok","mode":"demo"}`, 302, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests int
			transport := healthTransport(func(*http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": {"/elsewhere"}},
					Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			err := checkHealth("http://127.0.0.1:8080/healthz", transport)
			if (err == nil) != tc.wantOK || requests != 1 {
				t.Fatalf("health result = %v after %d requests, want ok=%t and one request", err, requests, tc.wantOK)
			}
		})
	}
}
