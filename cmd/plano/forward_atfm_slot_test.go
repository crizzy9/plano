package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	postInput := func(input io.Reader, signingKey []byte, want int) {
		t.Helper()
		exit, err := forwardATFMSlot(ctx, input, endpoint, signingKey, transport)
		if exit != want || (err != nil) != (want != 0) {
			t.Fatalf("relay exit %d (%v), want %d", exit, err, want)
		}
	}
	post := func(slot feeds.ATFMSlot, signingKey []byte, want int) {
		t.Helper()
		raw, err := json.Marshal(slot)
		if err != nil {
			t.Fatal(err)
		}
		body := bytes.NewReader(raw)
		input, status, err := atfmSlotRelayInput(body, nil, now)
		if err != nil || status != 0 || input != body {
			t.Fatalf("JSON relay did not preserve its input: %d (%v)", status, err)
		}
		postInput(input, signingKey, want)
	}
	get := func(token string, want *feeds.ATFMSlot) {
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
		if (len(got.ATFMSlots) != 0) != (want != nil) {
			t.Fatalf("unexpected slot visibility: %+v", got.ATFMSlots)
		}
		if want != nil && (len(got.ATFMSlots) != 1 || !got.ATFMSlots[0].TakeoffAt.Equal(*want.TakeoffAt) ||
			got.ATFMSlots[0].Source != want.Source || got.ATFMSlots[0].Reason != want.Reason ||
			!got.ATFMSlots[0].ObservedAt.Equal(want.ObservedAt) || strings.Contains(w.Body.String(), `"providerFlightId"`)) {
			t.Fatalf("unexpected public slot fields: %s", w.Body)
		}
	}
	post(slot, bytes.Repeat([]byte("b"), 32), relayPermission)
	get(ownerToken, nil)
	post(slot, key, 0)
	get(ownerToken, &slot)
	get(otherToken, nil)
	withdraw := slot
	withdraw.Status, withdraw.TakeoffAt, withdraw.ObservedAt = feeds.ATFMWithdrawn, nil, now.Add(-10*time.Second)
	post(withdraw, key, 0)
	post(slot, key, 0)
	get(ownerToken, nil)

	identity := fmt.Sprintf("-ARCID UAL88 -IFPLID ZZ12345678 -ADEP KSFO -ADES KEWR -EOBD %s -EOBT %s",
		scheduled.Format("060102"), scheduled.Format("1504"))
	postADEXP := func(message string, observedAt string, date ...string) {
		t.Helper()
		args := append([]string{"eurocontrol-adexp", flight.ID, "ZZ12345678", scheduled.Format(time.RFC3339), observedAt}, date...)
		input, status, err := atfmSlotRelayInput(strings.NewReader(message), args, time.Now().UTC())
		if err != nil || status != 0 {
			t.Fatalf("ADEXP conversion exit %d: %v", status, err)
		}
		postInput(input, key, 0)
	}
	euroTakeoff := takeoff.Truncate(time.Minute)
	euro := feeds.ATFMSlot{Source: feeds.Eurocontrol, TakeoffAt: &euroTakeoff, ObservedAt: now.Add(-8 * time.Second)}
	sam := fmt.Sprintf("-TITLE SAM %s -CTOT %s -CTOD %s -TIMESTAMP %s", identity,
		euroTakeoff.Format("1504"), euroTakeoff.Format("060102"), euro.ObservedAt.Format("060102150405"))
	postADEXP(sam, "-")
	get(ownerToken, &euro)
	get(otherToken, nil)
	euroTakeoff = euroTakeoff.Add(15 * time.Minute)
	euro.ObservedAt = now.Add(-5 * time.Second)
	postADEXP("-TITLE SRM "+identity+" -NEWCTOT "+euroTakeoff.Format("1504"),
		euro.ObservedAt.Format(time.RFC3339), euroTakeoff.Format(time.DateOnly))
	get(ownerToken, &euro)
	postADEXP("-TITLE SLC "+identity+" -REASON OUTREG", now.Add(-2*time.Second).Format(time.RFC3339))
	postADEXP(sam, "-")
	get(ownerToken, nil)

	igtd := scheduled.Truncate(time.Minute)
	path := filepath.Join(t.TempDir(), "binding.json")
	binding, err := json.Marshal(feeds.TFMSATFMSlotBinding{
		Version: 1, ProviderFlightID: flight.ID, OriginICAO: "KSFO", DestinationICAO: "KEWR",
		FlightRef: "1234567", AircraftID: "UAL88", SourceOrigin: "SFO", SourceDestination: "EWR",
		ScheduledDeparture: scheduled, IGTD: igtd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, binding, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"tfms-flight-control", path}
	postTFMS := func(message string) {
		t.Helper()
		input, status, err := atfmSlotRelayInput(strings.NewReader(message), args, time.Now().UTC())
		if err != nil || status != 0 || input == nil {
			t.Fatalf("FlightControl conversion exit %d: %v", status, err)
		}
		postInput(input, key, 0)
	}
	faa := feeds.ATFMSlot{Source: feeds.FAASWIM, TakeoffAt: &takeoff, ObservedAt: now.Add(-4 * time.Second), Reason: "Ground delay program"}
	active := tfmsRelayFixture(igtd, faa.ObservedAt, takeoff, "A", "CONTROL_ACTIVE")
	postTFMS(active)
	get(ownerToken, &faa)
	get(otherToken, nil)
	takeoff = takeoff.Add(15 * time.Minute)
	faa.ObservedAt = now.Add(-3 * time.Second)
	postTFMS(tfmsRelayFixture(igtd, faa.ObservedAt, takeoff, "A", "CONTROL_ACTIVE"))
	get(ownerToken, &faa)
	postTFMS(tfmsRelayFixture(igtd, now.Add(-time.Second), takeoff, "A", "CONTROL_CANCELED"))
	postTFMS(active)
	get(ownerToken, nil)
	t.Setenv(advisoryIngressKeyFile, "")
	status, err := runATFMSlotRelay(ctx, strings.NewReader(tfmsRelayFixture(igtd, now, takeoff, "R", "CONTROL_ACTIVE")), args)
	if err != nil || status != 0 {
		t.Fatalf("restricted FlightControl tried to forward: %d (%v)", status, err)
	}
	get(ownerToken, nil)

	for _, disabled := range []api.Server{{}, {Demo: true, AdvisoryIngressKey: key}} {
		w := httptest.NewRecorder()
		disabled.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, endpoint, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("disabled slot callback returned %d", w.Code)
		}
	}
}

func tfmsRelayFixture(igtd, observed, takeoff time.Time, sensitivity, indicator string) string {
	return fmt.Sprintf(`<ds:tfmDataService xmlns:ds="urn:us:gov:dot:faa:atm:tfm:tfmdataservice"
 xmlns:fd="urn:us:gov:dot:faa:atm:tfm:flightdata" xmlns:cm="urn:us:gov:dot:faa:atm:tfm:flightdatacommonmessages"
 xmlns:ce="urn:us:gov:dot:faa:atm:tfm:tfmdatacoreelements">
 <ds:fltdOutput><fd:fltdMessage msgType="FlightControl" fdTrigger="TMI_UPDATE" sourceFacility="TFMS"
   flightRef="1234567" acid="UAL88" depArpt="SFO" arrArpt="EWR" sensitivity="%s" sourceTimeStamp="%s">
   <fd:ncsmFlightControl>
     <cm:qualifiedAircraftId><ce:aircraftId>UAL88</ce:aircraftId><ce:igtd>%s</ce:igtd>
       <ce:departurePoint><ce:airport>KSFO</ce:airport></ce:departurePoint>
       <ce:arrivalPoint><ce:airport>KEWR</ce:airport></ce:arrivalPoint>
     </cm:qualifiedAircraftId>
     <cm:controlIndicator>%s</cm:controlIndicator>
     <cm:ncsmControlData><cm:ctd>%s</cm:ctd><cm:controlProgramType>GDP</cm:controlProgramType></cm:ncsmControlData>
   </fd:ncsmFlightControl>
 </fd:fltdMessage></ds:fltdOutput>
</ds:tfmDataService>`, sensitivity, observed.Format(time.RFC3339), igtd.Format(time.RFC3339), indicator, takeoff.Format(time.RFC3339))
}
