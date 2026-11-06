package api_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/api"
	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

const flightObservationCallbackPath = "/v1/webhooks/flight-observation"

func flightObservationBody(id, gate string, observed, departure time.Time, blocked bool) string {
	return fmt.Sprintf(`{"version":1,"observedAt":%q,"flight":{
		"fa_flight_id":%q,"ident":"UAL88","ident_icao":"UAL88","ident_iata":"UA88","status":"Scheduled",
		"origin":{"code":"KSFO","code_icao":"KSFO","code_iata":"SFO","timezone":"America/Los_Angeles"},
		"destination":{"code":"KEWR","code_icao":"KEWR","code_iata":"EWR","timezone":"America/New_York"},
		"blocked":%t,"cancelled":false,"diverted":false,"registration":null,"aircraft_type":null,"route_distance":null,
		"inbound_fa_flight_id":null,"atc_ident":null,"gate_origin":%q,"gate_destination":null,"terminal_origin":null,
		"terminal_destination":null,"baggage_claim":null,"scheduled_out":%q,"scheduled_off":null,"scheduled_in":%q,
		"scheduled_on":null,"estimated_out":null,"estimated_in":null,"actual_out":null,"actual_off":null,"actual_on":null,"actual_in":null
	}}`, observed.Format(time.RFC3339Nano), id, blocked, gate, departure.Format(time.RFC3339Nano), departure.Add(6*time.Hour).Format(time.RFC3339Nano))
}

func flightObservationRequest(key []byte, stamp, signedBody, postedBody string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, flightObservationCallbackPath, strings.NewReader(postedBody))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set("X-Plano-Timestamp", stamp)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(stamp + "." + signedBody))
	r.Header.Set("X-Plano-Signature", hex.EncodeToString(mac.Sum(nil)))
	return r
}

func TestFlightObservationCallbackRejectsInvalidRequestsAndAppliesOnce(t *testing.T) {
	ctx := t.Context()
	store, err := core.Open(filepath.Join(t.TempDir(), "flight-observation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	departure := now.Add(30 * time.Hour)
	prior := feeds.Flight{ID: "UAL88-tracked", Source: "flightaware_aeroapi", Number: "UA88", Origin: "SFO", OriginICAO: "KSFO",
		Destination: "EWR", DestinationICAO: "KEWR", Status: "scheduled", DepartureGate: "A2", ObservedAt: now.Add(-time.Minute), ScheduledDeparture: &departure}
	trackTestFlight(t, store, prior)
	untracked := prior
	untracked.ID = "UAL88-untracked"
	if err := store.Observe(ctx, untracked, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("f", 32))
	// A nil source makes this test independent of AeroAPI and ensures the route
	// can apply a snapshot without any additional provider request.
	handler := (api.Server{Store: store, FlightObservationKey: key, Logger: slog.New(slog.DiscardHandler)}).Handler()
	stamp := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	body := flightObservationBody(prior.ID, "B3", now, departure, false)
	request := func(body string) *http.Request { return flightObservationRequest(key, stamp(now), body, body) }
	call := func(r *http.Request, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want || want == http.StatusNoContent && w.Body.Len() != 0 {
			t.Fatalf("flight observation returned %d, want %d: %s", w.Code, want, w.Body)
		}
		if strings.Contains(w.Body.String(), prior.ID) {
			t.Fatal("response disclosed flight identity")
		}
	}
	assertFlight := func(gate string, observed time.Time, eventCount int) {
		t.Helper()
		details, err := store.Details(ctx, prior.ID)
		if err != nil || details.Flight.DepartureGate != gate || !details.Flight.ObservedAt.Equal(observed) || len(details.Events) != eventCount {
			t.Fatalf("unexpected flight state: %+v, %v", details, err)
		}
		if eventCount == 1 && (details.Events[0].Kind != "departure_gate" || details.Events[0].Body != "UA88 · B3") {
			t.Fatalf("unexpected flight event: %+v", details.Events[0])
		}
	}
	for _, at := range []time.Time{now.Add(-10 * time.Minute), now.Add(10 * time.Minute)} {
		call(flightObservationRequest(key, stamp(at), body, body), http.StatusUnauthorized)
	}
	call(flightObservationRequest(key, "+"+stamp(now), body, body), http.StatusUnauthorized)
	call(flightObservationRequest(key, stamp(now), body, body+" "), http.StatusUnauthorized)
	unsigned := request(body)
	unsigned.Header.Del("X-Plano-Signature")
	call(unsigned, http.StatusUnauthorized)
	wrongType := request(body)
	wrongType.Header.Set("Content-Type", "text/plain")
	call(wrongType, http.StatusUnsupportedMediaType)
	call(request(strings.Repeat(" ", feeds.MaxFlightObservationBytes+1)), http.StatusRequestEntityTooLarge)
	call(request(strings.Replace(body, `"gate_destination":null,`, "", 1)), http.StatusBadRequest)
	call(request(flightObservationBody(prior.ID, "B3", now.Add(-6*time.Minute), departure, false)), http.StatusBadRequest)
	call(request(flightObservationBody(prior.ID, "B3", now, departure, true)), http.StatusNoContent)
	call(request(flightObservationBody(untracked.ID, "B3", now, departure, false)), http.StatusNoContent)
	call(request(flightObservationBody("UAL88-unknown", "B3", now, departure, false)), http.StatusNoContent)
	continuation := strings.NewReplacer("KSFO", "KORD", `"SFO"`, `"ORD"`).Replace(body)
	call(request(continuation), http.StatusNoContent)
	if _, err := store.Flight(ctx, "UAL88-unknown"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown observation entered the cache: %v", err)
	}
	if got, err := store.Flight(ctx, untracked.ID); err != nil || !got.ObservedAt.Equal(prior.ObservedAt) {
		t.Fatalf("untracked flight updated: %+v, %v", got, err)
	}
	assertFlight("A2", prior.ObservedAt, 0)
	call(request(body), http.StatusNoContent)
	call(request(body), http.StatusNoContent)
	call(request(flightObservationBody(prior.ID, "C4", now.Add(-time.Second), departure, false)), http.StatusNoContent)
	assertFlight("B3", now, 1)
}

func TestFlightObservationCallbackRequiresLiveConfiguration(t *testing.T) {
	for _, service := range []api.Server{{}, {Demo: true, FlightObservationKey: []byte(strings.Repeat("f", 32))}} {
		w := httptest.NewRecorder()
		service.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, flightObservationCallbackPath, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("disabled callback returned %d", w.Code)
		}
	}
}
