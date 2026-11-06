package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestBuiltInConnectionMapsUseReportedAirportCodes(t *testing.T) {
	var rules *ConnectionRules
	for _, tc := range []struct{ iata, icao, mapURL string }{
		{"SEA", "KSEA", "https://www.portseattle.org/sea/maps"},
		{"DFW", "KDFW", "https://www.dfwairport.com/map/"},
		{"ATL", "KATL", "https://www.atl.com/maps/"},
		{"SFO", "KSFO", "https://www.flysfo.com/maps"},
		{"EWR", "KEWR", "https://www.newarkairport.com/explore-ewr/airport-map"},
		{"JFK", "KJFK", "https://www.jfkairport.com/explore-jfk/airport-map"},
		{"LAX", "KLAX", "https://www.flylax.com/lax-terminal-maps"},
		{"ORD", "KORD", "https://maps.ord.flychicago.com/"},
	} {
		t.Run(tc.iata, func(t *testing.T) {
			for _, codes := range []struct{ primary, icao string }{{primary: strings.ToLower(tc.iata)}, {icao: tc.icao}} {
				in := feeds.Flight{Destination: codes.primary, DestinationICAO: codes.icao}
				out := feeds.Flight{Origin: codes.primary, OriginICAO: codes.icao}
				route, configured := rules.routeForConnection(in, out)
				if configured || route.MapURL != tc.mapURL || route.GuideURL != "" || len(route.Instructions) != 0 {
					t.Fatalf("unexpected map fallback %+v, configured=%v", route, configured)
				}
			}
			in := feeds.Flight{Destination: tc.iata}
			out := feeds.Flight{Origin: tc.iata, OriginICAO: "KXZZ"}
			if route, _ := rules.routeForConnection(in, out); route.MapURL != "" {
				t.Fatalf("conflicting airport received map %+v", route)
			}
		})
	}
}

func TestBuiltInConnectionDirectionsRetainGeneralTiming(t *testing.T) {
	now := time.Now()
	arrive, depart := now.Add(time.Hour), now.Add(3*time.Hour)
	for _, tc := range []struct {
		airport, arrivalGate, departureGate, arrivalTerminal, departureTerminal, instruction string
		transfer                                                                             int
	}{
		{airport: "SEA", arrivalGate: "N12", departureGate: "S4", instruction: "Yellow Line", transfer: 50},
		{airport: "DFW", arrivalGate: "A12", departureGate: "D9", arrivalTerminal: "A", departureTerminal: "D",
			instruction: "Skylink", transfer: 65},
		{airport: "ATL", arrivalGate: "T4", departureGate: "F3", instruction: "Plane Train", transfer: 50},
	} {
		t.Run(tc.airport, func(t *testing.T) {
			in := feeds.Flight{Destination: tc.airport, ArrivalGate: tc.arrivalGate, ArrivalTerminal: tc.arrivalTerminal, ScheduledArrival: &arrive}
			out := feeds.Flight{Origin: tc.airport, DepartureGate: tc.departureGate, DepartureTerminal: tc.departureTerminal, ScheduledDeparture: &depart}
			assess := func() connectionResult {
				t.Helper()
				result, _, err := (Server{}).assessConnection(t.Context(), in, out, now, func(_ context.Context, _ feeds.Flight, arrival bool, _ time.Time) (*core.GatePrediction, error) {
					if arrival && in.ArrivalGate == "" {
						return &core.GatePrediction{Gate: tc.arrivalGate, Samples: 5, Share: 0.8}, nil
					}
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			got := assess()
			directions := strings.Join(got.Instructions, " ")
			if got.Precision != "generic" || got.TransferMinutes != tc.transfer || got.BoardingReserveMinutes != defaultConnectionBoardingReserveMinutes ||
				got.ForecastTransferMinutes != nil || got.MapURL == "" || got.GuideURL == "" || !strings.Contains(directions, "stay in the secure area") ||
				!strings.Contains(directions, tc.instruction) || !strings.Contains(directions, "passport control") {
				t.Fatalf("unexpected connection with official directions: %+v", got)
			}
			in.ArrivalGate, in.ArrivalTerminal = "", ""
			got = assess()
			if got.ArrivalGatePrediction == nil || got.GuideURL != "" || got.MapURL == "" || strings.Contains(strings.Join(got.Instructions, " "), tc.instruction) {
				t.Fatalf("historical gate changed directions: %+v", got)
			}
		})
	}
}

func TestBuiltInConnectionDirectionsFallBackWithoutUsableEndpoints(t *testing.T) {
	var rules *ConnectionRules
	for _, tc := range []struct{ name, gate, inboundStatus, outboundStatus string }{
		{name: "gate conflicts with terminal", gate: "B12"},
		{name: "unrecognized terminal F gate", gate: "F12"},
		{name: "diverted inbound", gate: "A12", inboundStatus: "diverted"},
		{name: "outbound already departed", gate: "A12", outboundStatus: "taxi_out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{Destination: "DFW", ArrivalTerminal: "A", ArrivalGate: tc.gate, Status: tc.inboundStatus}
			out := feeds.Flight{Origin: "DFW", DepartureTerminal: "D", DepartureGate: "D9", Status: tc.outboundStatus}
			if route, configured := rules.routeForConnection(in, out); configured || route.MapURL == "" || route.GuideURL != "" || len(route.Instructions) != 0 {
				t.Fatalf("unusable endpoints received directions %+v, configured=%v", route, configured)
			}
		})
	}
}

func TestBuiltInConnectionGuidesRespectOperatorGraphs(t *testing.T) {
	const operatorMap = "https://airport.example/terminal-maps"
	const officialMap = "https://www.dfwairport.com/map/"
	for _, tc := range []struct {
		name, mapURL, wantMap string
		configured            bool
	}{
		{name: "directed operator path", mapURL: operatorMap, wantMap: operatorMap, configured: true},
		{name: "operator path with official map fallback", wantMap: officialMap, configured: true},
		{name: "deliberately disconnected reverse path", mapURL: operatorMap, wantMap: operatorMap},
		{name: "disconnected graph with official map fallback", wantMap: officialMap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := strings.ReplaceAll(connectionFixture, "XZZ", "DFW")
			if tc.mapURL == "" {
				fixture = strings.Replace(fixture, `"mapUrl": "https://airport.example/terminal-maps",`, "", 1)
			}
			rules, err := LoadConnectionRules(strings.NewReader(fixture))
			if err != nil {
				t.Fatal(err)
			}
			fromTerminal, fromGate, toTerminal, toGate := "A", "A1", "B", "B1"
			if !tc.configured {
				fromTerminal, fromGate, toTerminal, toGate = "B", "B1", "A", "A1"
			}
			in := feeds.Flight{Destination: "DFW", ArrivalTerminal: fromTerminal, ArrivalGate: fromGate}
			out := feeds.Flight{Origin: "DFW", DepartureTerminal: toTerminal, DepartureGate: toGate}
			route, configured := rules.routeForConnection(in, out)
			if configured != tc.configured || route.MapURL != tc.wantMap || route.GuideURL != "" {
				t.Fatalf("unexpected operator result %+v, configured=%v", route, configured)
			}
			if configured && (route.TransferMinutes != 30 || route.Precision != "gates" ||
				!strings.Contains(strings.Join(route.Instructions, " "), "transfer security")) ||
				!configured && len(route.Instructions) != 0 {
				t.Fatalf("operator route replaced by the built-in guide: %+v", route)
			}
		})
	}
}

func TestBuiltInConnectionGuideFromTrackedFlights(t *testing.T) {
	ctx := t.Context()
	store, err := core.Open(filepath.Join(t.TempDir(), "plano.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	token, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	arrive := now.Add(time.Hour)
	depart := arrive.Add(64 * time.Minute)
	for _, flight := range []feeds.Flight{
		{ID: "inbound", Number: "PL101", Origin: "SFO", Destination: "SEA", DestinationICAO: "KSEA", ArrivalGate: "N12", ScheduledArrival: &arrive, ObservedAt: now, Status: "scheduled"},
		{ID: "outbound", Number: "PL202", Origin: "SEA", OriginICAO: "KSEA", Destination: "JFK", DepartureGate: "S4", ScheduledDeparture: &depart, ObservedAt: now, Status: "scheduled"},
	} {
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	handler := (Server{Store: store, Logger: slog.New(slog.DiscardHandler)}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/v1/connections?inbound=inbound&outbound=outbound", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("connection response %d: %s", response.Code, response.Body)
	}
	var got connectionResult
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Risk != "Risky" || got.AvailableMinutes != 64 || got.TransferMinutes != 50 ||
		got.BoardingReserveMinutes != 15 || got.Precision != "generic" ||
		got.MapURL != "https://www.portseattle.org/sea/maps" || got.GuideURL != "https://www.portseattle.org/blog/all-aboard-sea-underground" ||
		!strings.Contains(strings.Join(got.Instructions, " "), "Yellow Line") {
		t.Fatalf("unexpected tracked connection response: %+v", got)
	}
}
