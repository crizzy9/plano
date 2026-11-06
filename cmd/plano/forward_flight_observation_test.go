package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const flightObservationRelayFixtureURL = "https://relay.invalid/v1/webhooks/flight-observation"

func flightObservationRelayBody(observed time.Time, blocked bool) []byte {
	return []byte(fmt.Sprintf(` {
  "version": 1, "observedAt": %q, "flight": {
    "fa_flight_id": "fixture-UA123", "ident": "UAL123", "ident_iata": "UA123", "ident_icao": "UAL123",
    "status": "Scheduled", "blocked": %t, "cancelled": false, "diverted": false,
    "origin": {"code":"KSFO", "code_icao":"KSFO", "code_iata":"SFO", "timezone":"America/Los_Angeles"},
    "destination": {"code":"KORD", "code_icao":"KORD", "code_iata":"ORD", "timezone":"America/Chicago"},
    "scheduled_out": %q, "scheduled_off": null, "scheduled_in": %q, "scheduled_on": null,
    "estimated_out": null, "estimated_in": null, "actual_out": null, "actual_off": null, "actual_on": null, "actual_in": null,
    "registration": null, "aircraft_type": null, "route_distance": null, "inbound_fa_flight_id": null, "atc_ident": null,
    "gate_origin": null, "gate_destination": null, "terminal_origin": null, "terminal_destination": null, "baggage_claim": null
  }
}
`, observed.UTC().Format(time.RFC3339), blocked, observed.Add(time.Hour).UTC().Format(time.RFC3339), observed.Add(5*time.Hour).UTC().Format(time.RFC3339)))
}

func TestForwardFlightObservationSignsExactBody(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	raw := flightObservationRelayBody(time.Now().Add(-time.Second), false)
	for _, tc := range []struct{ status, exit int }{
		{http.StatusNoContent, 0},
		{http.StatusBadRequest, relayDataError},
		{http.StatusUnauthorized, relayPermission},
		{http.StatusTemporaryRedirect, relayConfigError},
		{http.StatusTooManyRequests, relayTempFailure},
		{http.StatusBadGateway, relayTempFailure},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			requests := 0
			responseBody := &relayResponseBody{}
			transport := relayTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				defer r.Body.Close()
				got, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(got, raw) || r.ContentLength != int64(len(raw)) {
					t.Fatal("relay did not preserve the exact observation bytes")
				}
				if r.Method != http.MethodPost || r.URL.String() != flightObservationRelayFixtureURL ||
					r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "" {
					t.Fatal("unexpected flight observation request metadata")
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Fatal("missing flight observation request deadline")
				}
				stamp := r.Header.Get("X-Plano-Timestamp")
				epoch, err := strconv.ParseInt(stamp, 10, 64)
				if err != nil || time.Since(time.Unix(epoch, 0)).Abs() > 10*time.Second {
					t.Fatal("missing or stale flight observation signature timestamp")
				}
				mac := hmac.New(sha256.New, key)
				mac.Write([]byte(stamp + "."))
				mac.Write(raw)
				if r.Header.Get("X-Plano-Signature") != hex.EncodeToString(mac.Sum(nil)) {
					t.Fatal("signature did not cover the exact timestamp and observation")
				}
				return &http.Response{
					StatusCode: tc.status,
					Header:     http.Header{"Location": {"https://redirect.invalid"}},
					Body:       responseBody,
				}, nil
			})
			status, err := forwardFlightObservation(t.Context(), bytes.NewReader(raw), flightObservationRelayFixtureURL, key, transport)
			if status != tc.exit || (err == nil) != (status == 0) || requests != 1 || responseBody.read || !responseBody.closed {
				t.Fatalf("exit = %d, error = %v, requests = %d, response read = %t, closed = %t", status, err, requests, responseBody.read, responseBody.closed)
			}
		})
	}
}

func TestForwardFlightObservationChecksInputBeforeTransport(t *testing.T) {
	now := time.Now()
	key := bytes.Repeat([]byte("k"), 32)
	for _, tc := range []struct {
		name string
		raw  []byte
		exit int
	}{
		{"malformed", []byte(`{"version":`), relayDataError},
		{"oversized", bytes.Repeat([]byte("x"), feeds.MaxFlightObservationBytes+1), relayDataError},
		{"stale", flightObservationRelayBody(now.Add(-6*time.Minute), false), relayDataError},
		{"future", flightObservationRelayBody(now.Add(time.Minute), false), relayDataError},
		{"blocked", flightObservationRelayBody(now.Add(-time.Second), true), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := relayTransport(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid or blocked flight observation reached the transport")
				return nil, nil
			})
			status, err := forwardFlightObservation(t.Context(), bytes.NewReader(tc.raw), flightObservationRelayFixtureURL, key, transport)
			if status != tc.exit || (err == nil) != (status == 0) {
				t.Fatalf("exit = %d, error = %v, want %d", status, err, tc.exit)
			}
		})
	}
}
