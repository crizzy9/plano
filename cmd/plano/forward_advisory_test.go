package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const advisoryRelayFixtureURL = "https://relay.invalid/v1/webhooks/advisory"

func relayAdvisoryBody(id string, observed time.Time) []byte {
	return []byte(fmt.Sprintf(" {\n  \"version\": 1, \"source\": \"faa_swim\", \"advisory\": {\"id\": %q, \"airport\": \"KEWR\", \"kind\": \"ground_stop\", \"observedAt\": %q}\n}\r\n",
		id, observed.UTC().Format(time.RFC3339)))
}

func TestForwardAdvisorySignsExactBodyAndClassifiesResponse(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	raw := relayAdvisoryBody("fixture", time.Now())
	for _, tc := range []struct {
		status, exit int
	}{
		{http.StatusNoContent, 0},
		{http.StatusBadRequest, 65},
		{http.StatusRequestEntityTooLarge, 65},
		{http.StatusUnauthorized, 77},
		{http.StatusForbidden, 77},
		{http.StatusTemporaryRedirect, 78},
		{http.StatusRequestTimeout, 75},
		{http.StatusTooManyRequests, 75},
		{http.StatusBadGateway, 75},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			requests := 0
			responseBody := &relayResponseBody{}
			transport := relayTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				defer r.Body.Close()
				got, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(got, raw) || r.ContentLength != int64(len(raw)) {
					t.Fatal("relay did not preserve the exact input bytes")
				}
				if r.Method != http.MethodPost || r.URL.String() != advisoryRelayFixtureURL ||
					r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "" {
					t.Fatal("unexpected advisory relay request metadata")
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Fatal("missing advisory request deadline")
				}
				stamp := r.Header.Get("X-Plano-Timestamp")
				epoch, err := strconv.ParseInt(stamp, 10, 64)
				if err != nil || time.Since(time.Unix(epoch, 0)).Abs() > 10*time.Second {
					t.Fatal("missing or stale advisory signature timestamp")
				}
				mac := hmac.New(sha256.New, key)
				mac.Write([]byte(stamp + "."))
				mac.Write(raw)
				if r.Header.Get("X-Plano-Signature") != hex.EncodeToString(mac.Sum(nil)) {
					t.Fatal("advisory signature did not cover the exact timestamp and body")
				}
				return &http.Response{
					StatusCode: tc.status,
					Header:     http.Header{"Location": {"https://redirect.invalid"}},
					Body:       responseBody,
				}, nil
			})
			status, err := forwardAdvisory(t.Context(), bytes.NewReader(raw), advisoryRelayFixtureURL, key, nil, transport)
			if status != tc.exit || (err == nil) != (status == 0) || requests != 1 || responseBody.read || !responseBody.closed {
				t.Fatalf("status = %d, error = %v, requests = %d, response read = %t, closed = %t", status, err, requests, responseBody.read, responseBody.closed)
			}
		})
	}
}

func TestForwardAdvisoryValidatesWholeBatchBeforeDelivery(t *testing.T) {
	now := time.Now()
	good := relayAdvisoryBody("valid-fixture", now)
	key := bytes.Repeat([]byte("k"), 32)
	for _, tc := range []struct {
		name   string
		bodies [][]byte
	}{
		{"empty", nil},
		{"too many", make([][]byte, maxAdvisoryRelayNotices+1)},
		{"invalid final envelope", [][]byte{good, []byte("{")}},
		{"oversized final envelope", [][]byte{good, bytes.Repeat([]byte(" "), feeds.MaxAdvisoryIngressBytes+1)}},
		{"stale final envelope", [][]byte{good, relayAdvisoryBody("stale", now.Add(-7*time.Hour))}},
		{"future final envelope", [][]byte{good, relayAdvisoryBody("future", now.Add(time.Minute))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			convert := func(io.Reader) ([][]byte, error) { return tc.bodies, nil }
			transport := relayTransport(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid batch reached the transport")
				return nil, nil
			})
			status, err := forwardAdvisory(t.Context(), nil, advisoryRelayFixtureURL, key, convert, transport)
			if status != 65 || err == nil {
				t.Fatalf("got exit %d, %v for invalid batch", status, err)
			}
		})
	}
}

func TestForwardAdvisoryStopsAtFirstFailedDelivery(t *testing.T) {
	now := time.Now()
	bodies := [][]byte{relayAdvisoryBody("first", now), relayAdvisoryBody("second", now), relayAdvisoryBody("third", now)}
	key := bytes.Repeat([]byte("k"), 32)
	convert := func(io.Reader) ([][]byte, error) { return bodies, nil }
	requests := 0
	transport := relayTransport(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(raw, bodies[requests]) {
			t.Fatal("relay did not preserve converter order")
		}
		requests++
		if requests == 2 {
			return nil, errors.New("sensitive fixture content")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})
	status, err := forwardAdvisory(t.Context(), nil, advisoryRelayFixtureURL, key, convert, transport)
	if status != 75 || requests != 2 || err == nil || err.Error() != "could not reach the advisory relay endpoint" {
		t.Fatalf("got exit %d, %v after %d requests", status, err, requests)
	}
}

func TestForwardAdvisoryDefersFutureEffectiveConversion(t *testing.T) {
	convert := func(io.Reader) ([][]byte, error) {
		return nil, fmt.Errorf("sensitive fixture detail: %w", feeds.ErrAdvisoryNotYetEffective)
	}
	transport := relayTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("future-effective advisory reached the transport")
		return nil, nil
	})
	status, err := forwardAdvisory(t.Context(), nil, advisoryRelayFixtureURL, bytes.Repeat([]byte("k"), 32), convert, transport)
	if status != 75 || err == nil || err.Error() != "advisory is not yet effective" {
		t.Fatalf("got exit %d, %v for future-effective conversion", status, err)
	}
	current := relayAdvisoryBody("current-fixture", time.Now())
	convert = func(io.Reader) ([][]byte, error) { return [][]byte{current}, feeds.ErrAdvisoryNotYetEffective }
	requests := 0
	transport = relayTransport(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, current) {
			t.Fatal("relay sent a different advisory")
		}
		requests++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})
	status, err = forwardAdvisory(t.Context(), nil, advisoryRelayFixtureURL, bytes.Repeat([]byte("k"), 32), convert, transport)
	if status != 75 || err == nil || requests != 1 {
		t.Fatalf("got exit %d, %v after %d current notices", status, err, requests)
	}
}

func TestForwardAdvisoryRequiresFileCredential(t *testing.T) {
	t.Setenv("PLANO_ADVISORY_RELAY_URL", advisoryRelayFixtureURL)
	t.Setenv("PLANO_ADVISORY_INGRESS_KEY_FILE", "")
	if status, _ := forwardAdvisoryFromEnv(t.Context(), strings.NewReader(""), nil); status != 78 {
		t.Fatalf("accepted missing key file, exit %d", status)
	}
	path := filepath.Join(t.TempDir(), "fixture-key")
	if err := os.WriteFile(path, bytes.Repeat([]byte("k"), 31), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLANO_ADVISORY_INGRESS_KEY_FILE", path)
	if status, _ := forwardAdvisoryFromEnv(t.Context(), strings.NewReader(""), nil); status != 78 {
		t.Fatalf("accepted short key, exit %d", status)
	}
	if err := os.WriteFile(path, []byte(" \n"+strings.Repeat("k", 32)+"\n "), 0600); err != nil {
		t.Fatal(err)
	}
	if status, _ := forwardAdvisoryFromEnv(t.Context(), strings.NewReader(""), nil); status != 65 {
		t.Fatalf("did not reach input validation with valid key file, exit %d", status)
	}
}
