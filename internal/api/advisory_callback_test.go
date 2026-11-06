package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func signedAdvisoryRequest(key []byte, now time.Time, body string) *http.Request {
	stamp := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(stamp + "." + body))
	request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/advisory", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Plano-Timestamp", stamp)
	request.Header.Set("X-Plano-Signature", hex.EncodeToString(mac.Sum(nil)))
	return request
}

func TestSignedAdvisoryBodyReadFailures(t *testing.T) {
	service := newImportTestServer(t)
	service.AdvisoryIngressKey = []byte(strings.Repeat("t", 32))
	handler := service.Handler()
	now := time.Now().UTC().Truncate(time.Second)
	const privateMarker = "fixture-private-detail"
	body := fmt.Sprintf(`{"version":1,"source":"faa_swim","advisory":{"id":"retry-fixture","airport":"KSFO","kind":"departure_delay","direction":"departure","reason":%q,"observedAt":%q,"endsAt":%q}}`,
		privateMarker, now.Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
	flight := feeds.Flight{Status: "scheduled", Origin: "SFO", OriginICAO: "KSFO", ScheduledDeparture: new(now.Add(10 * time.Minute))}
	for _, tc := range []struct {
		name string
		body string
		err  error
		want int
	}{
		{name: "over limit", body: body + strings.Repeat(" ", feeds.MaxAdvisoryIngressBytes-len(body)+1), want: http.StatusRequestEntityTooLarge},
		{name: "timed out", body: body, err: os.ErrDeadlineExceeded, want: http.StatusRequestTimeout},
		{name: "interrupted", body: body, err: io.ErrUnexpectedEOF, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := signedAdvisoryRequest(service.AdvisoryIngressKey, now, tc.body)
			if tc.err != nil {
				// Even a complete, correctly signed prefix must not apply if
				// the upload fails before EOF.
				readError := fmt.Errorf("%s: %w", privateMarker, tc.err)
				request.Body = io.NopCloser(io.MultiReader(request.Body, iotest.ErrReader(readError)))
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want || strings.Contains(response.Body.String(), privateMarker) {
				t.Fatalf("signed advisory returned %d, want %d with a sanitized response: %s", response.Code, tc.want, response.Body)
			}
			notices, err := service.Store.FlightNotices(t.Context(), flight, now)
			if err != nil || len(notices) != 0 {
				t.Fatalf("failed upload applied notices: %+v, error %v", notices, err)
			}
		})
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, signedAdvisoryRequest(service.AdvisoryIngressKey, now, body))
	notices, err := service.Store.FlightNotices(t.Context(), flight, now)
	if response.Code != http.StatusNoContent || err != nil || len(notices) != 1 {
		t.Fatalf("complete retry returned %d and notices %+v, error %v", response.Code, notices, err)
	}
}

func TestSignedAdvisoryTFMSObservationAge(t *testing.T) {
	service := newImportTestServer(t)
	service.AdvisoryIngressKey = []byte(strings.Repeat("t", 32))
	handler := service.Handler()
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name, id string
		age, end time.Duration
		status   int
	}{
		{"active long program", "tfms/gdp/ORD", 8 * time.Hour, time.Hour, http.StatusNoContent},
		{"ended revision", "tfms/gdp/ORD", 7 * time.Hour, -time.Minute, http.StatusNoContent},
		{"past extended cap", "tfms/gdp/ORD", 49 * time.Hour, time.Hour, http.StatusBadRequest},
		{"ordinary stale record", "ordinary-gdp", 8 * time.Hour, time.Hour, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := now.Add(-tc.age)
			body := fmt.Sprintf(`{"version":1,"source":"faa_swim","advisory":{"id":%q,"airport":"ORD","kind":"ground_delay","direction":"arrival","airportContextOnly":true,"observedAt":%q,"startsAt":%q,"endsAt":%q}}`,
				tc.id, observed.Format(time.RFC3339), observed.Add(time.Minute).Format(time.RFC3339), now.Add(tc.end).Format(time.RFC3339))
			request := signedAdvisoryRequest(service.AdvisoryIngressKey, now, body)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("signed advisory returned %d, want %d: %s", response.Code, tc.status, response.Body)
			}
		})
	}
}
