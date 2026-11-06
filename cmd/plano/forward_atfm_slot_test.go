package main

import (
	"bytes"
	"encoding/json"
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

func TestForwardATFMSlotToTrackedFlight(t *testing.T) {
	store, err := core.Open(filepath.Join(t.TempDir(), "atfm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	scheduled := now.Add(20 * time.Minute)
	takeoff := scheduled.Add(35 * time.Minute)
	flight := feeds.Flight{ID: "UAL88-atfm-fixture", Source: "flightaware_aeroapi", Number: "UA88",
		Origin: "SFO", OriginICAO: "KSFO", Destination: "EWR", DestinationICAO: "KEWR",
		Status: "scheduled", ScheduledDeparture: &scheduled, ObservedAt: now.Add(-time.Minute)}
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
	otherToken, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte("a"), 32)
	service := api.Server{Store: store, AdvisoryIngressKey: key, Logger: slog.New(slog.DiscardHandler)}
	handler := service.Handler()
	const endpoint = "https://plano.invalid/v1/webhooks/atfm-slot"
	transport := relayTransport(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})
	slot := feeds.ATFMSlot{Version: 1, Source: feeds.FAASWIM, ProviderFlightID: flight.ID,
		OriginICAO: flight.OriginICAO, DestinationICAO: flight.DestinationICAO, ScheduledDeparture: scheduled,
		ObservedAt: now.Add(-20 * time.Second), Status: feeds.ATFMAssigned, TakeoffAt: &takeoff, Reason: "Arrival capacity reduced by thunderstorms"}
	post := func(slot feeds.ATFMSlot, signingKey []byte, want int) {
		t.Helper()
		raw, err := json.Marshal(slot)
		if err != nil {
			t.Fatal(err)
		}
		exit, err := forwardATFMSlot(ctx, bytes.NewReader(raw), endpoint, signingKey, transport)
		if exit != want || (err != nil) != (want != 0) {
			t.Fatalf("relay exit %d (%v), want %d", exit, err, want)
		}
	}
	get := func(token string, visible bool) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/v1/flights/"+flight.ID, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var got struct {
			Flight    feeds.Flight
			ATFMSlots []core.ATFMSlotView
			Events    []core.Event
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
			t.Fatalf("detail %d (%v) = %s", w.Code, err, w.Body)
		}
		if got.Flight.EstimatedDeparture != nil || got.Flight.ScheduledDeparture == nil || !got.Flight.ScheduledDeparture.Equal(scheduled) ||
			!got.Flight.ObservedAt.Equal(flight.ObservedAt) {
			t.Fatalf("slot changed the flight snapshot: %+v", got.Flight)
		}
		for _, event := range got.Events {
			if event.Kind != "check_in" {
				t.Fatalf("unexpected timeline event: %+v", event)
			}
		}
		if (len(got.ATFMSlots) != 0) != visible {
			t.Fatalf("slot visibility = %t, want %t", len(got.ATFMSlots) != 0, visible)
		}
		if visible && (len(got.ATFMSlots) != 1 || !got.ATFMSlots[0].TakeoffAt.Equal(takeoff) || got.ATFMSlots[0].Source != feeds.FAASWIM ||
			got.ATFMSlots[0].Reason != slot.Reason || strings.Contains(w.Body.String(), `"providerFlightId"`)) {
			t.Fatalf("unexpected public slot fields: %s", w.Body)
		}
	}
	post(slot, bytes.Repeat([]byte("b"), 32), relayPermission)
	get(ownerToken, false)
	post(slot, key, 0)
	get(ownerToken, true)
	get(otherToken, false)
	withdraw := slot
	withdraw.Status, withdraw.TakeoffAt, withdraw.ObservedAt = feeds.ATFMWithdrawn, nil, now.Add(-10*time.Second)
	post(withdraw, key, 0)
	post(slot, key, 0)
	get(ownerToken, false)
	for _, disabled := range []api.Server{{}, {Demo: true, AdvisoryIngressKey: key}} {
		w := httptest.NewRecorder()
		disabled.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, endpoint, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("disabled slot callback returned %d", w.Code)
		}
	}
}
