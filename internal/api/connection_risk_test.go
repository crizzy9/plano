package api

import (
	"context"
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

func TestConnectionAssessmentBoundsDepartureAfterMovement(t *testing.T) {
	now := time.Date(2026, time.November, 5, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) *time.Time {
		stamp := now.Add(time.Duration(minutes) * time.Minute)
		return &stamp
	}
	arrival := feeds.Flight{Destination: "XZZ", Status: "arrived", ActualArrival: at(-35)}
	for _, tc := range []struct {
		name     string
		onward   feeds.Flight
		window   int
		risk     string
		guidance bool
	}{
		{"future ETD after taxi-out", feeds.Flight{Status: "taxi_out", EstimatedDeparture: at(65), ObservedAt: *at(-1)}, 34, "Missed", false},
		{"known gate-out", feeds.Flight{Status: "airborne", ActualDeparture: at(-9), EstimatedDeparture: at(65), ObservedAt: *at(-1)}, 26, "Missed", false},
		{"known landing but no gate-out", feeds.Flight{Status: "landed", ActualLanding: at(-15), EstimatedDeparture: at(65), ObservedAt: *at(-1)}, 20, "Missed", false},
		{"future observation bounded by clock", feeds.Flight{Status: "airborne", ScheduledDeparture: at(65), ObservedAt: *at(1)}, 35, "Missed", false},
		{"upcoming ETD", feeds.Flight{Status: "boarding", EstimatedDeparture: at(65), ObservedAt: *at(-1)}, 100, "Relaxed", true},
		{"cancelled onward", feeds.Flight{Status: "cancelled", EstimatedDeparture: at(65), ObservedAt: *at(-1)}, 100, "Missed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.onward.Origin = "XZZ"
			result, assessment, err := (Server{}).assessConnection(t.Context(), arrival, tc.onward, now,
				func(context.Context, feeds.Flight, bool, time.Time) (*core.GatePrediction, error) { return nil, nil })
			if err != nil || result.Risk != tc.risk || result.AvailableMinutes != tc.window || result.GuidanceAvailable != tc.guidance ||
				assessment.Risk != tc.risk || assessment.Monitorable != tc.guidance {
				t.Fatalf("assessment = %+v, monitor = %+v, error = %v", result, assessment, err)
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

func TestConnectionRoutesAndForecastsUseSharedICAO(t *testing.T) {
	airportGraph := func(code, mapURL, instruction string, base, forecast int) string {
		return fmt.Sprintf(`{
			"codes":[%q], "mapUrl":%q,
			"nodes":[
				{"id":"terminal-a", "label":"Terminal A", "terminal":"A"},
				{"id":"a1", "label":"Gate A1", "terminal":"A", "gate":"A1"},
				{"id":"b1", "label":"Gate B1", "terminal":"B", "gate":"B1"}
			],
			"edges":[
				{"from":"terminal-a", "to":"b1", "minutes":%d, "instruction":%q},
				{"from":"a1", "to":"b1", "minutes":%d, "instruction":"Forecast gate route."}
			]
		}`, code, mapURL, base, instruction, forecast)
	}
	const iataMap, icaoMap = "https://iata.example/map", "https://icao.example/map"
	const iataInstruction, icaoInstruction = "Follow the IATA graph route.", "Follow the ICAO graph route."
	iataGraph := airportGraph("XZZ", iataMap, iataInstruction, 14, 20)
	bothGraphs := iataGraph + "," + airportGraph("KABC", icaoMap, icaoInstruction, 30, 55)
	laxGraph := airportGraph("LAX", iataMap, iataInstruction, 14, 20)
	klaxGraph := airportGraph("KLAX", iataMap, iataInstruction, 14, 20)
	now := time.Date(2026, time.November, 5, 12, 0, 0, 0, time.UTC)
	arrival, departure := now.Add(time.Hour), now.Add(110*time.Minute)
	predictArrivalGate := func(_ context.Context, _ feeds.Flight, arrival bool, _ time.Time) (*core.GatePrediction, error) {
		if arrival {
			return &core.GatePrediction{Gate: "A1"}, nil
		}
		return nil, nil
	}
	type routeExpectation struct {
		airport, mapURL, instruction string
		base, forecast, reserve      int
		precision, risk              string
	}
	verifiedRoute := routeExpectation{"KABC", icaoMap, icaoInstruction, 30, 55, 0, "mixed", "Risky"}
	agreedRoute := verifiedRoute
	agreedRoute.airport = "XZZ"
	genericRoute := routeExpectation{"KABC", "", "Follow connections signage at KABC.", 65, 0, 15, "generic", "Risky"}
	iataRoute := routeExpectation{"XZZ", iataMap, iataInstruction, 14, 20, 0, "mixed", "Relaxed"}
	for _, tc := range []struct {
		name, inCode, outCode, graphs string
		want                          routeExpectation
	}{
		{"conflicting display codes use the ICAO graph", "XZZ", "ABC", bothGraphs, verifiedRoute},
		{"matching display codes still use the ICAO graph", "XZZ", "XZZ", bothGraphs, agreedRoute},
		{"conflicting display codes without ICAO graph use generic timing", "XZZ", "ABC", iataGraph, genericRoute},
		{"missing inbound display code leaves only the common ICAO", "", "XZZ", iataGraph, genericRoute},
		{"missing outbound display code leaves only the common ICAO", "XZZ", "", iataGraph, genericRoute},
		{"matching display codes allow IATA-only graph", "XZZ", "XZZ", iataGraph, iataRoute},
		{"outbound uses the common ICAO as its display code", "XZZ", "KABC", iataGraph, iataRoute},
		{"inbound uses the common ICAO as its display code", "KABC", "XZZ", iataGraph, iataRoute},
		{"matching IATA identifies a different known airport", "LAX", "LAX", laxGraph, genericRoute},
		{"alternate ICAO identifies a different known airport", "KABC", "KLAX", klaxGraph, genericRoute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rules, err := LoadConnectionRules(strings.NewReader(`{"version":1,"airports":[` + tc.graphs + `]}`))
			if err != nil {
				t.Fatal(err)
			}
			in := feeds.Flight{Destination: tc.inCode, DestinationICAO: "KABC", ArrivalTerminal: "A", ScheduledArrival: &arrival}
			out := feeds.Flight{Origin: tc.outCode, OriginICAO: "KABC", DepartureTerminal: "B", DepartureGate: "B1", ScheduledDeparture: &departure}
			result, assessment, err := (Server{ConnectionRules: rules}).assessConnection(t.Context(), in, out, now, predictArrivalGate)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if result.Airport != want.airport || result.MapURL != want.mapURL || result.TransferMinutes != want.base ||
				result.BoardingReserveMinutes != want.reserve || result.Precision != want.precision ||
				!slices.Contains(result.Instructions, want.instruction) || slices.Contains(result.Instructions, "Forecast gate route.") {
				t.Fatalf("displayed route = %+v, want %+v", result, want)
			}
			if want.forecast == 0 {
				if result.ForecastTransferMinutes != nil {
					t.Fatalf("generic fallback used a configured gate forecast: %d", *result.ForecastTransferMinutes)
				}
			} else if result.ForecastTransferMinutes == nil || *result.ForecastTransferMinutes != want.forecast {
				t.Fatalf("forecast from the wrong graph: %+v, want %d min", result, want.forecast)
			}
			if result.Risk != want.risk || assessment.Risk != want.risk {
				t.Fatalf("risk = %s, monitor risk = %s, want %s", result.Risk, assessment.Risk, want.risk)
			}
		})
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
