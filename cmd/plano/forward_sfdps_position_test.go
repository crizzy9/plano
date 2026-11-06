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

func TestForwardSFDPSPositionToPrivateRadar(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := core.Open(filepath.Join(dir, "sfdps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	scheduled, takeoff, arrival := now.Add(-time.Hour), now.Add(-45*time.Minute), now.Add(time.Hour)
	fix := now.Add(-18*time.Second + 250*time.Millisecond)
	flight := feeds.Flight{
		ID: "UAL88-sfdps-fixture", Source: "flightaware_aeroapi", Number: "UA88", TailNumber: "N888AA",
		Origin: "SFO", Destination: "EWR", OriginICAO: "KSFO", DestinationICAO: "KEWR", Status: "airborne",
		ScheduledDeparture: &scheduled, ActualTakeoff: &takeoff, ScheduledArrival: &arrival, ObservedAt: now,
	}
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
	const gufi = "e2747c10-1cd0-4e18-9320-9ac72c4b1ad3"
	bindingPath := filepath.Join(dir, "binding.json")
	binding := fmt.Sprintf(`{"version":1,"displayAllowed":true,"providerFlightId":%q,"originICAO":"KSFO","destinationICAO":"KEWR","tailNumber":%q,"scheduledDeparture":%q,"actualTakeoff":%q,"gufi":%q,"aircraftId":"UAL88"}`,
		flight.ID, flight.TailNumber, scheduled.Format(time.RFC3339), takeoff.Format(time.RFC3339), gufi)
	if err := os.WriteFile(bindingPath, []byte(binding), 0o600); err != nil {
		t.Fatal(err)
	}
	xml := fmt.Sprintf(`<nas:MessageCollection xmlns:nas="http://www.faa.aero/nas/3.0" xmlns:fb="http://www.fixm.aero/base/3.0" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <message xsi:type="nas:FlightMessageType">
    <flight source="TH" timestamp=%q xsi:type="nas:NasFlightType">
      <aircraftDescription registration="n888aa"/>
      <arrival arrivalPoint="KEWR" xsi:type="nas:NasArrivalType"/>
      <departure departurePoint="KSFO" xsi:type="nas:NasDepartureType">
        <runwayPositionAndTime><runwayTime><actual time=%q/></runwayTime></runwayPositionAndTime>
      </departure>
      <enRoute xsi:type="nas:NasEnRouteType">
        <position positionTime=%q reportSource="SURVEILLANCE" targetPositionTime=%q xsi:type="nas:NasAircraftPositionType">
          <actualSpeed><surveillance uom="KNOTS">451.0</surveillance></actualSpeed>
          <altitude uom="FEET">34000.0</altitude>
          <position xsi:type="fb:LocationPointType">
            <location srsName="urn:ogc:def:crs:EPSG::4326"><pos>39.08 -119.02</pos></location>
          </position>
          <targetAltitude uom="FEET">36000.0</targetAltitude>
          <targetPosition srsName="urn:ogc:def:crs:EPSG::4326"><pos>40.09 -118.03</pos></targetPosition>
        </position>
      </enRoute>
      <flightIdentification aircraftIdentification="UAL88" xsi:type="nas:NasFlightIdentificationType"/>
      <flightStatus fdpsFlightStatus="ACTIVE" xsi:type="nas:NasFlightStatusType"/>
      <gufi codeSpace="urn:uuid">%s</gufi>
    </flight>
  </message>
</nas:MessageCollection>`, now.Format(time.RFC3339), takeoff.Format(time.RFC3339), fix.Format(time.RFC3339Nano), fix.Add(-time.Second).Format(time.RFC3339Nano), gufi)
	mismatch := strings.Replace(xml, gufi, "054291e5-5644-4e30-adc9-dbc643e52d6a", 1)
	if input, status, err := sfdpsPositionRelayInput(strings.NewReader(mismatch), bindingPath, now); input != nil || status != 0 || err != nil {
		t.Fatalf("unmatched GUFI should be ignored: input present %v, status %d, error %v", input != nil, status, err)
	}
	mismatch = strings.Replace(xml, `registration="n888aa"`, `registration="N777AA"`, 1)
	if input, status, err := sfdpsPositionRelayInput(strings.NewReader(mismatch), bindingPath, now); input != nil || status != 0 || err != nil {
		t.Fatalf("mismatched reported registration should be ignored: input present %v, status %d, error %v", input != nil, status, err)
	}
	input, status, err := sfdpsPositionRelayInput(strings.NewReader(xml), bindingPath, now)
	if input == nil || status != 0 || err != nil {
		t.Fatalf("SFDPS normalization returned input present %v, status %d, error %v", input != nil, status, err)
	}
	normalized, err := io.ReadAll(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(normalized, []byte(`"tailNumber":"N888AA"`)) {
		t.Fatalf("SFDPS relay omitted the bound registration: %s", normalized)
	}
	key := bytes.Repeat([]byte("s"), 32)
	handler := (api.Server{Store: store, SWIMPositionKey: key, Logger: slog.New(slog.DiscardHandler)}).Handler()
	posted := false
	transport := relayTransport(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		posted = true
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("signed callback returned %d: %s", w.Code, w.Body)
		}
		return w.Result(), nil
	})
	if status, err := swimPositionRelay.forward(ctx, bytes.NewReader(normalized), "https://plano.invalid"+swimPositionRelay.path, key, transport); status != 0 || err != nil || !posted {
		t.Fatalf("relay returned status %d, error %v, invoked transport %v", status, err, posted)
	}
	type radarPage struct {
		SWIMPositions []feeds.Position
		SWIMCursor    string
		SWIMRevision  string
	}
	get := func(token string, wantStatus int) radarPage {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/v1/flights/"+flight.ID+"/radar?provider=1&swim=1", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != wantStatus {
			t.Fatalf("radar returned %d, want %d: %s", w.Code, wantStatus, w.Body)
		}
		var page radarPage
		if wantStatus == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
		}
		return page
	}
	ownerPage := get(ownerToken, http.StatusOK)
	if len(ownerPage.SWIMPositions) != 1 || ownerPage.SWIMCursor != fix.Format(time.RFC3339Nano) || ownerPage.SWIMRevision == "" {
		t.Fatalf("unexpected owner radar page: %+v", ownerPage)
	}
	if p := ownerPage.SWIMPositions[0]; p.Source != feeds.FAASWIM || p.Kind != "radar" || p.Latitude != 39.08 || p.Longitude != -119.02 ||
		!p.At.Equal(fix) || p.AltitudeFeet == nil || *p.AltitudeFeet != 34000 || p.GroundSpeedKnots == nil || *p.GroundSpeedKnots != 451 {
		t.Fatalf("radar did not preserve the reported fix: %+v", p)
	}
	if page := get(foreignToken, http.StatusOK); len(page.SWIMPositions) != 0 || page.SWIMCursor != "" || page.SWIMRevision != "" {
		t.Fatalf("foreign installation got the private radar fix: %+v", page)
	}
	get("", http.StatusUnauthorized)
}
