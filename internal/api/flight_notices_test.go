package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestFlightNoticesScopeAndWithdrawalAfterSignedIngress(t *testing.T) {
	service := newImportTestServer(t)
	service.AdvisoryIngressKey = []byte(strings.Repeat("n", 32))
	token, owner := newImportTestInstallation(t, service.Store)
	otherToken, _ := newImportTestInstallation(t, service.Store)
	now := time.Now().UTC().Truncate(time.Second)
	flight := feeds.Flight{ID: "notice-flight", Number: "UA123", Origin: "SFO", OriginICAO: "KSFO", Destination: "EWR", DestinationICAO: "KEWR",
		Status: "scheduled", ScheduledDeparture: new(now.Add(30 * time.Minute)), ScheduledArrival: new(now.Add(6 * time.Hour)), ObservedAt: now}
	if err := service.Store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	handler := service.Handler()
	post := func(airport, direction, raw, id string, observed, end time.Time) {
		t.Helper()
		body := fmt.Sprintf(`{"version":1,"source":"faa_swim","advisory":{"id":%q,"airport":%q,"kind":"taxiway_closure","direction":%q,"rawReason":%q,"observedAt":%q,"endsAt":%q}}`,
			id, airport, direction, raw, observed.Format(time.RFC3339), end.Format(time.RFC3339))
		stamp := strconv.FormatInt(now.Unix(), 10)
		mac := hmac.New(sha256.New, service.AdvisoryIngressKey)
		mac.Write([]byte(stamp + "." + body))
		request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/advisory", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Plano-Timestamp", stamp)
		request.Header.Set("X-Plano-Signature", hex.EncodeToString(mac.Sum(nil)))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("signed advisory returned %d: %s", response.Code, response.Body)
		}
	}
	// A notice that arrives before a traveler starts tracking still has context.
	post("KSFO", "departure", "!SFO SFO TWY A CLSD", "taxiway-a", now.Add(-time.Minute), now.Add(time.Hour))
	post("KSFO", "arrival", "!SFO SFO TWY B CLSD", "arrivals-only", now, now.Add(time.Hour))
	if err := service.Store.Track(t.Context(), owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	path := "/v1/flights/" + flight.ID
	get := func(token string) []core.FlightNotice {
		t.Helper()
		body := importTestRequest(t, handler, "GET", path, token, "", 200)
		if strings.Contains(body, "rawReason") || strings.Contains(body, "TWY A CLSD") {
			t.Fatalf("notice returned unexpanded source text: %s", body)
		}
		var result struct{ Notices []core.FlightNotice }
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			t.Fatal(err)
		}
		return result.Notices
	}
	notices := get(token)
	if len(notices) != 1 || notices[0].Airport != "KSFO" || notices[0].Operation != "departure" || notices[0].Summary != "Taxiway A closed" {
		t.Fatalf("unexpected flight notices: %+v", notices)
	}
	if foreign := get(otherToken); len(foreign) != 0 {
		t.Fatalf("another installation received flight notices: %+v", foreign)
	}
	post("KSFO", "departure", "!SFO SFO TWY A CLSD", "taxiway-a", now, now.Add(-time.Second))
	if withdrawn := get(token); len(withdrawn) != 0 {
		t.Fatalf("expired revision left the earlier notice visible: %+v", withdrawn)
	}
}
