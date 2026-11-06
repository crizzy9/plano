package api

import (
	"bytes"
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

func TestAIXMAdvisoriesReachFlightNoticesAndRetireClosure(t *testing.T) {
	service := newImportTestServer(t)
	service.AdvisoryIngressKey = []byte(strings.Repeat("a", 32))
	token, owner := newImportTestInstallation(t, service.Store)
	now := time.Now().UTC().Truncate(time.Second)
	start, end := now.Add(-10*time.Minute), now.Add(time.Hour)
	flight := feeds.Flight{ID: "aixm-fixture", Number: "UA123", Origin: "SFO", OriginICAO: "KSFO",
		Destination: "EWR", DestinationICAO: "KEWR", Status: "scheduled", ObservedAt: now,
		ScheduledDeparture: new(now.Add(15 * time.Minute)), ScheduledArrival: new(now.Add(6 * time.Hour))}
	if err := service.Store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.Track(t.Context(), owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	handler := service.Handler()
	post := func(number, action, extra string, issued time.Time) {
		t.Helper()
		xml := fmt.Sprintf(`<NOTAM xmlns="http://www.aixm.aero/schema/5.1.1/event"><series>A</series><number>%s</number><year>%d</year><type>%s</type><issued>%s</issued><scope>A</scope><location>KSFO</location><effectiveStart>%s</effectiveStart>%s</NOTAM>`,
			number, now.Year(), action, issued.Format(time.RFC3339), start.Format("0601021504"), extra)
		bodies, err := feeds.AIXMAdvisoryBodies(strings.NewReader(xml), feeds.FAASWIM, "ksfo-fixture", time.Time{}, now)
		if err != nil || len(bodies) != 1 {
			t.Fatalf("convert: got %d envelopes, error %v", len(bodies), err)
		}
		stamp := strconv.FormatInt(now.Unix(), 10)
		mac := hmac.New(sha256.New, service.AdvisoryIngressKey)
		mac.Write([]byte(stamp + "."))
		mac.Write(bodies[0])
		request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/advisory", bytes.NewReader(bodies[0]))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Plano-Timestamp", stamp)
		request.Header.Set("X-Plano-Signature", hex.EncodeToString(mac.Sum(nil)))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("signed advisory returned %d: %s", response.Code, response.Body)
		}
	}
	get := func() []core.FlightNotice {
		t.Helper()
		var detail struct{ Notices []core.FlightNotice }
		body := importTestRequest(t, handler, http.MethodGet, "/v1/flights/"+flight.ID, token, "", http.StatusOK)
		if err := json.Unmarshal([]byte(body), &detail); err != nil {
			t.Fatal(err)
		}
		return detail.Notices
	}
	validity := "<effectiveEnd>" + end.Format("0601021504") + "</effectiveEnd>"
	post("10", "N", validity+"<text>TWY B CLSD</text>", now.Add(-2*time.Minute))
	if notices := get(); len(notices) != 1 || notices[0].Kind != feeds.AdvisoryAirportNotice || notices[0].Summary != "Taxiway B closed" {
		t.Fatalf("unexpected taxiway context: %+v", notices)
	}
	post("11", "N", validity+"<text>AD closed.</text>", now.Add(-time.Minute))
	if notices := get(); len(notices) != 2 || notices[0].Kind != "airport_closure" {
		t.Fatalf("missing full closure: %+v", notices)
	}
	detail, err := service.Store.DetailsForInstallation(t.Context(), owner, flight.ID)
	if err != nil {
		t.Fatal(err)
	}
	var closureAlerts int
	for _, event := range detail.Events {
		if event.Kind == feeds.AdvisoryAirportNotice {
			t.Fatal("taxiway context generated an alert")
		}
		if event.Kind == "airport_closure" {
			closureAlerts++
		}
	}
	if closureAlerts != 1 {
		t.Fatalf("got %d closure alerts, want 1", closureAlerts)
	}
	post("12", "C", fmt.Sprintf("<referredSeries>A</referredSeries><referredNumber>11</referredNumber><referredYear>%d</referredYear>", now.Year()), now)
	if notices := get(); len(notices) != 1 || notices[0].Kind != feeds.AdvisoryAirportNotice {
		t.Fatalf("cancellation left closure visible: %+v", notices)
	}
}

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
