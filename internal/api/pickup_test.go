package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestPickupDoesNotReuseOriginalGateForDiversionOrCancellation(t *testing.T) {
	now := time.Now().UTC()
	arrival := now.Add(time.Hour)
	f := feeds.Flight{Number: "XY100", Origin: "LAX", Destination: "EWR", ArrivalGate: "C12", ArrivalTerminal: "C", Baggage: "4", ScheduledArrival: &arrival, ObservedAt: now}
	for _, status := range []string{"cancelled", "diverted"} {
		f.Status = status
		view := pickupView(f, now, false)
		if view.Arrival != "" || view.Flight.ArrivalGate != "" || view.Flight.ArrivalTerminal != "" || view.Flight.Baggage != "" {
			t.Fatalf("misleading pickup detail for %s: %#v", status, view)
		}
		if status == "diverted" && view.Notice == "" {
			t.Fatal("diversion missing context")
		}
	}
}

func TestPickupRendersCachedProviderTrackInline(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	store := openPassportStore(t, filepath.Join(t.TempDir(), "pickup.db"))
	_, owner := passportInstallation(t, store)
	takeoff, estimatedArrival := now.Add(-12*time.Minute), now.Add(30*time.Minute)
	flight := feeds.Flight{ID: "XY100-fixture", Number: "XY100", Origin: "SFO", Destination: "EWR", Status: "airborne",
		Source: "flightaware_aeroapi", ObservedAt: now, ActualTakeoff: &takeoff, EstimatedArrival: &estimatedArrival}
	positions := []feeds.Position{
		{At: now.Add(-7 * time.Minute), Latitude: 38.0, Longitude: -121.1, Source: flight.Source},
		{At: now.Add(-4 * time.Minute), Latitude: 38.9, Longitude: -120.4, Source: flight.Source},
	}
	if err := store.Observe(t.Context(), flight, positions, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(t.Context(), owner, flight.ID, "32A"); err != nil {
		t.Fatal(err)
	}
	link, token, err := store.CreatePickupLink(t.Context(), owner, flight.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	handler := (Server{Store: store}).Handler()
	open := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/share/"+token, nil))
		return response
	}
	response := open()
	if response.Code != http.StatusOK {
		t.Fatalf("pickup link returned %d: %s", response.Code, response.Body)
	}
	page := response.Body.String()
	for _, want := range []string{"Estimated gate arrival", "Reported flight path", "Latest recorded position at " + positions[1].At.Format("15:04 UTC"),
		`href="#pickup-land"`, `class="route-track" d="M`, `class="route-latest"`} {
		if !strings.Contains(page, want) {
			t.Errorf("pickup page missing %q", want)
		}
	}
	if strings.Contains(page, "32A") || strings.Contains(page, token) || strings.Contains(page, "<script") || strings.Contains(page, "ZgotmplZ") {
		t.Fatal("pickup page contains a private or unsupported value")
	}
	style := sha256.Sum256([]byte(pickupStyle))
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" ||
		!strings.Contains(response.Header().Get("Content-Security-Policy"), "default-src 'none'; style-src 'sha256-"+base64.StdEncoding.EncodeToString(style[:])+"'") {
		t.Fatal("pickup route lost its browser isolation headers")
	}
	if err := store.RevokePickupLink(t.Context(), owner, link.ID); err != nil {
		t.Fatal(err)
	}
	if revoked := open(); revoked.Code != http.StatusNotFound || strings.Contains(revoked.Body.String(), "route-latest") {
		t.Fatalf("revoked pickup link returned %d", revoked.Code)
	}
}
