package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/api"
	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestForwardSWIMPositionToPrivateRadar(t *testing.T) {
	ctx := t.Context()
	store, err := core.Open(filepath.Join(t.TempDir(), "swim.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	scheduled, takeoff, fix := now.Add(-time.Hour), now.Add(-45*time.Minute), now.Add(-time.Second+25*time.Nanosecond)
	flight := feeds.Flight{
		ID: "UAL88-swim-fixture", Source: "flightaware_aeroapi", Number: "UA88", TailNumber: "N888AA",
		Origin: "SFO", Destination: "EWR", OriginICAO: "KSFO", DestinationICAO: "KEWR", Status: "airborne",
		ScheduledDeparture: &scheduled, ActualTakeoff: &takeoff, ObservedAt: now,
	}
	arrival := now.Add(time.Hour)
	flight.ScheduledArrival = &arrival
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	ownerToken, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, ownerToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	foreignToken, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte("s"), 32)
	logger := slog.New(slog.DiscardHandler)
	service := api.Server{Store: store, SWIMPositionKey: key, Logger: logger}
	handler := service.Handler()
	const path = "/v1/webhooks/swim-position"
	const endpoint = "https://plano.invalid" + path
	body := fmt.Sprintf(`{"version":1,"displayAllowed":true,"providerFlightId":%q,"originICAO":"KSFO","destinationICAO":"KEWR","scheduledDeparture":%q,"actualTakeoff":%q,"positions":[{"at":%q,"latitude":0,"longitude":-119.02,"altitudeFeet":32000}]}`,
		flight.ID, scheduled.Format(time.RFC3339), takeoff.Format(time.RFC3339), fix.Format(time.RFC3339Nano))
	send := func(raw string, signingKey []byte, alter bool, wantHTTP, wantExit int) {
		t.Helper()
		called := false
		transport := relayTransport(func(r *http.Request) (*http.Response, error) {
			called = true
			posted, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil || string(posted) != raw {
				t.Fatalf("relay changed signed body: %v", err)
			}
			if alter {
				posted = append(posted, ' ')
			}
			r.Body = io.NopCloser(bytes.NewReader(posted))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != wantHTTP {
				t.Fatalf("callback returned %d, want %d", w.Code, wantHTTP)
			}
			return w.Result(), nil
		})
		status, err := forwardSWIMPosition(ctx, strings.NewReader(raw), endpoint, signingKey, transport)
		if status != wantExit || (err != nil) != (wantExit != 0) || called != (wantHTTP != 0) {
			t.Fatalf("relay returned %d (%v), invoked transport %v", status, err, called)
		}
	}
	get := func(token, query string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/v1/flights/"+flight.ID+"/radar?"+query, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("radar returned %d, want %d", w.Code, status)
		}
		return w
	}
	for _, disabled := range []api.Server{{Logger: logger}, {Demo: true, SWIMPositionKey: key, Logger: logger}} {
		w := httptest.NewRecorder()
		disabled.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		if w.Code != http.StatusNotFound {
			t.Fatalf("disabled callback returned %d", w.Code)
		}
	}
	send(strings.Replace(body, `"displayAllowed":true`, `"displayAllowed":false`, 1), key, false, 0, relayDataError)
	send(strings.Replace(body, `"latitude":0,`, "", 1), key, false, 0, relayDataError)
	send(strings.Replace(body, fix.Format(time.RFC3339Nano), now.Add(-6*time.Minute).Format(time.RFC3339), 1), key, false, 0, relayDataError)
	send(body, bytes.Repeat([]byte("x"), 32), false, http.StatusUnauthorized, relayPermission)
	send(body, key, true, http.StatusUnauthorized, relayPermission)
	send(strings.Replace(body, `"KSFO"`, `"KOAK"`, 1), key, false, http.StatusNoContent, 0)
	const query = "provider=1&swim=1"
	if raw := get(ownerToken, query, http.StatusOK).Body.String(); !strings.Contains(raw, `"swimPositions":[]`) {
		t.Fatalf("mismatched report reached private radar: %s", raw)
	}
	send(body, key, false, http.StatusNoContent, 0)
	send(body, key, false, http.StatusNoContent, 0)
	var page struct {
		SWIMPositions     []feeds.Position
		SWIMCursor        string
		SWIMRevision      string
		Positions         []feeds.Position
		ProviderPositions []feeds.Position
	}
	if err := json.Unmarshal(get(ownerToken, query, http.StatusOK).Body.Bytes(), &page); err != nil || len(page.SWIMPositions) != 1 ||
		page.SWIMCursor != fix.Format(time.RFC3339Nano) || page.SWIMRevision == "" || len(page.Positions)+len(page.ProviderPositions) != 0 {
		t.Fatalf("unexpected radar page: %+v (%v)", page, err)
	}
	if p := page.SWIMPositions[0]; p.Source != feeds.FAASWIM || p.Kind != "radar" || p.Latitude != 0 || !p.At.Equal(fix) {
		t.Fatalf("unexpected normalized position: %+v", p)
	}
	if raw := get(foreignToken, query, http.StatusOK).Body.String(); !strings.Contains(raw, `"swimPositions":[]`) || strings.Contains(raw, `"swimRevision"`) {
		t.Fatalf("foreign installation got private radar: %s", raw)
	}
	if raw := get(ownerToken, "provider=1", http.StatusOK).Body.String(); strings.Contains(raw, `"swimPositions"`) {
		t.Fatalf("legacy radar response changed: %s", raw)
	}
	for _, invalid := range []string{"swim=1", "provider=1&swim=2", "provider=1&swimAfter=bad", query + "&swimAfter=bad", query + "&swimRevision=-1"} {
		get(ownerToken, invalid, http.StatusBadRequest)
	}
	service.SWIMPositionKey = nil
	handler = service.Handler()
	if raw := get(ownerToken, query, http.StatusOK).Body.String(); strings.Contains(raw, `"swimPositions"`) {
		t.Fatalf("disabled integration returned stored SWIM positions: %s", raw)
	}
}
