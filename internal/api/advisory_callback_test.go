package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
			stamp := strconv.FormatInt(now.Unix(), 10)
			mac := hmac.New(sha256.New, service.AdvisoryIngressKey)
			mac.Write([]byte(stamp + "." + body))
			request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/advisory", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Plano-Timestamp", stamp)
			request.Header.Set("X-Plano-Signature", hex.EncodeToString(mac.Sum(nil)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("signed advisory returned %d, want %d: %s", response.Code, tc.status, response.Body)
			}
		})
	}
}
