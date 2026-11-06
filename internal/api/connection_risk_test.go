package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestConnectionRiskAccountsForBoardingAndActualMovement(t *testing.T) {
	now := time.Date(2026, time.November, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, inStatus, outStatus, want string
		arrival, departure, wantWindow  int
		actualIn, actualOut             bool
	}{
		{name: "transfer fits but boarding reserve does not", arrival: 25, departure: 65, wantWindow: 40, want: "Risky"},
		{name: "exact transfer and boarding reserve", arrival: 25, departure: 70, wantWindow: 45, want: "Tight"},
		{name: "transfer and reserve with extra time", arrival: 25, departure: 95, wantWindow: 70, want: "Relaxed"},
		{name: "stale estimated arrival no longer counts elapsed time", arrival: -30, departure: 40, wantWindow: 40, want: "Risky"},
		{name: "known gate-in counts time already available", arrival: -30, departure: 40, wantWindow: 70, actualIn: true, want: "Relaxed"},
		{name: "past departure without movement is stale", arrival: -100, departure: -5, wantWindow: 95, actualIn: true, want: "Risky"},
		{name: "onward departed while inbound lacked gate-in", arrival: -100, departure: -10, wantWindow: 90, outStatus: "airborne", want: "Missed"},
		{name: "actual gate times expose infeasible historical transfer", arrival: -45, departure: -5, wantWindow: 40, actualIn: true, actualOut: true, want: "Missed"},
		{name: "completed historical connection remains feasible", arrival: -85, departure: -5, wantWindow: 80, actualIn: true, actualOut: true, outStatus: "arrived", want: "Relaxed"},
		{name: "inbound diverted", arrival: 25, departure: 95, wantWindow: 70, inStatus: "diverted", want: "Missed"},
		{name: "outbound cancelled", arrival: 25, departure: 95, wantWindow: 70, outStatus: "cancelled", want: "Missed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arrival := now.Add(time.Duration(tc.arrival) * time.Minute)
			departure := now.Add(time.Duration(tc.departure) * time.Minute)
			in, out := feeds.Flight{Status: tc.inStatus}, feeds.Flight{Status: tc.outStatus}
			if tc.actualIn {
				in.ActualArrival = &arrival
			}
			if tc.actualOut {
				out.ActualDeparture = &departure
			}
			window, risk := connectionRisk(in, out, arrival, departure, now, 30+defaultConnectionBoardingReserveMinutes, connectionDeparted(out))
			if window != tc.wantWindow || risk != tc.want {
				t.Fatalf("got %d min and %s, want %d min and %s", window, risk, tc.wantWindow, tc.want)
			}
		})
	}
}

func TestConnectionAirportsUseVerifiedICAOFirst(t *testing.T) {
	in := feeds.Flight{Destination: "XZZ", DestinationICAO: "KXZZ"}
	if connectionAirportsMatch(in, feeds.Flight{Origin: "XZZ", OriginICAO: "KZXX"}) {
		t.Fatal("display codes hid conflicting ICAO codes")
	}
	if !connectionAirportsMatch(in, feeds.Flight{Origin: "XZZ"}) {
		t.Fatal("matching display codes were rejected when ICAO was unavailable")
	}
}

func TestConnectionForecastAffectsRiskWithoutReplacingRoute(t *testing.T) {
	ctx := t.Context()
	store, err := core.Open(filepath.Join(t.TempDir(), "plano.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rules, err := LoadConnectionRules(strings.NewReader(strings.Replace(connectionFixture, `"minutes":29`, `"minutes":8`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	arrival := now.Add(time.Hour)
	departure := arrival.Add(29 * time.Minute)
	in := feeds.Flight{ID: "arriving", Number: "PL101", Origin: "XYA", Destination: "XZZ", DestinationICAO: "KXZZ", ArrivalTerminal: "A", Status: "scheduled", ScheduledArrival: &arrival, ObservedAt: now}
	out := feeds.Flight{ID: "departing", Number: "PL202", Origin: "KXZZ", OriginICAO: "KXZZ", Destination: "XYB", DepartureTerminal: "B", DepartureGate: "B1", Status: "scheduled", ScheduledDeparture: &departure, ObservedAt: now}
	for _, flight := range []feeds.Flight{in, out} {
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	for day := 1; day <= 3; day++ {
		landed := now.AddDate(0, 0, -day)
		previous := in
		previous.ID = fmt.Sprintf("arrival-%d", day)
		previous.ArrivalGate, previous.Status = "A1", "arrived"
		previous.ScheduledArrival, previous.ActualArrival, previous.ObservedAt = &landed, &landed, landed
		if err := store.Observe(ctx, previous, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	handler := Server{Store: store, ConnectionRules: rules, Logger: slog.New(slog.DiscardHandler)}.Handler()
	getConnection := func() connectionResult {
		t.Helper()
		request := httptest.NewRequest("GET", "/v1/connections?inbound=arriving&outbound=departing", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != 200 {
			t.Fatalf("GET connection: %d %s", recorder.Code, recorder.Body)
		}
		var got connectionResult
		if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := getConnection()
	if got.Risk != "Risky" || got.AvailableMinutes != 29 || got.TransferMinutes != 23 || got.BoardingReserveMinutes != 0 ||
		got.ForecastTransferMinutes == nil || *got.ForecastTransferMinutes != 30 || got.Precision != "mixed" {
		t.Fatalf("unexpected planning estimate: %+v", got)
	}
	if got.ArrivalGate != "" || got.DepartureGate != "B1" || got.ArrivalGatePrediction == nil || got.ArrivalGatePrediction.Gate != "A1" ||
		got.MapURL != "https://airport.example/terminal-maps" || slices.Contains(got.Instructions, "Follow signs to transfer security.") {
		t.Fatalf("forecast leaked into the reported route: %+v", got)
	}
	in.ArrivalGate, in.ObservedAt = "A1", now.Add(time.Second)
	if err := store.Observe(ctx, in, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	got = getConnection()
	if got.ArrivalGate != "A1" || got.ArrivalGatePrediction != nil || got.ForecastTransferMinutes != nil ||
		got.Precision != "gates" || got.TransferMinutes != 30 || got.BoardingReserveMinutes != 0 || !slices.Contains(got.Instructions, "Follow signs to transfer security.") {
		t.Fatalf("reported gate did not supersede the forecast: %+v", got)
	}
	in.ArrivalGate, in.ArrivalTerminal, in.ObservedAt = "", "", now.Add(2*time.Second)
	laterDeparture := arrival.Add(64 * time.Minute)
	out.ScheduledDeparture, out.ObservedAt = &laterDeparture, now.Add(time.Second)
	for _, flight := range []feeds.Flight{in, out} {
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	got = getConnection()
	if got.Precision != "generic" || got.Risk != "Risky" || got.TransferMinutes != 50 || got.BoardingReserveMinutes != 15 ||
		got.ForecastTransferMinutes == nil || *got.ForecastTransferMinutes != 30 {
		t.Fatalf("forecast shortened the fallback with boarding reserve: %+v", got)
	}
}
