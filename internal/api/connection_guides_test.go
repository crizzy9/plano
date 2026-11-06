package api

import (
	"cmp"
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
		{"DEN", "KDEN", "https://maps.flydenver.com/"},
		{"AMS", "EHAM", "https://www.schiphol.nl/en/airport-maps/"},
		{"CDG", "LFPG", "https://www.parisaeroport.fr/en/passengers/at-the-airport/terminals-map/cdg"},
		{"FRA", "EDDF", "https://www.frankfurt-airport.com/en/at-the-airport/orientation/airport-map.html"},
		{"SIN", "WSSS", "https://www.changiairport.com/en/at-changi/map.html"},
		{"YYZ", "CYYZ", "https://www.torontopearson.com/en/while-you-are-here/terminal-maps"},
		{"HND", "RJTT", "https://tokyo-haneda.com/en/floor/index.html"},
		{"DXB", "OMDB", "https://dubaiairports.ae/express-maps"},
		{"DOH", "OTHH", "https://www.jal.co.jp/jp/en/inter/airport/doh/info/"},
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

func TestBuiltInSchengenTransferPlanning(t *testing.T) {
	now := time.Date(2027, time.March, 16, 12, 0, 0, 0, time.UTC)
	arrival := now.Add(10 * time.Minute)
	for _, tc := range []struct {
		name, airport, icao, arrivingTerminal, arrivingGate, departingTerminal, departingGate, destination string
		available, planning                                                                                int
		instruction                                                                                        string
	}{
		{"Schiphol Pier D border", "AMS", "EHAM", "1", "D65", "1", "D26", "BOS", 60, 80, "passport control"},
		{"CDG border and renamed gates", "CDG", "LFPG", "2F", "F26", "2E", "K35", "BOS", 85, 90, "Terminal 5, Gates D"},
		{"CDG remote terminal", "CDG", "LFPG", "2F", "F26", "7", "N31", "NTE", 105, 120, "secure Orange shuttle"},
		{"Frankfurt cross-terminal border", "FRA", "EDDF", "1", "A15", "3", "J12", "BOS", 79, 80, "passport control"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			departure := arrival.Add(time.Duration(tc.available) * time.Minute)
			in := feeds.Flight{Origin: "FRA", Destination: tc.airport, DestinationICAO: tc.icao, ArrivalTerminal: tc.arrivingTerminal,
				ArrivalGate: tc.arrivingGate, ScheduledArrival: &arrival}
			out := feeds.Flight{Origin: tc.airport, OriginICAO: tc.icao, Destination: tc.destination, DepartureTerminal: tc.departingTerminal,
				DepartureGate: tc.departingGate, ScheduledDeparture: &departure}
			got, _, err := (Server{}).assessConnection(t.Context(), in, out, now,
				func(context.Context, feeds.Flight, bool, time.Time) (*core.GatePrediction, error) { return nil, nil })
			instructions := strings.Join(got.Instructions, " ")
			if err != nil || got.Precision != "generic" || got.Risk != "Risky" || got.TransferMinutes+got.BoardingReserveMinutes != tc.planning ||
				got.BoardingReserveMinutes != defaultConnectionBoardingReserveMinutes || got.GuideURL == "" || got.MapURL == "" ||
				!strings.Contains(instructions, tc.instruction) || !strings.Contains(instructions, "Your reported departure gate is "+tc.departingGate+".") ||
				strings.Contains(instructions, "Continue to gate ") {
				t.Fatalf("unexpected transfer plan %+v, %v", got, err)
			}
		})
	}
}

func TestBuiltInConnectionPlanningAfterDeparture(t *testing.T) {
	now := time.Date(2026, time.November, 6, 18, 0, 0, 0, time.UTC)
	arrival, departure := now.Add(-90*time.Minute), now.Add(-25*time.Minute)
	in := feeds.Flight{Destination: "LHR", ArrivalTerminal: "5", ActualArrival: &arrival}
	out := feeds.Flight{Origin: "LHR", DepartureTerminal: "5", ActualDeparture: &departure}
	got, assessment, err := (Server{}).assessConnection(t.Context(), in, out, now,
		func(context.Context, feeds.Flight, bool, time.Time) (*core.GatePrediction, error) { return nil, nil })
	if err != nil || got.Risk != "Missed" || got.AvailableMinutes != 65 || got.TransferMinutes != 65 ||
		got.BoardingReserveMinutes != 15 || got.MapURL == "" || got.GuideURL != "" || assessment.Monitorable ||
		strings.Contains(strings.Join(got.Instructions, " "), "Flight Connections") {
		t.Fatalf("unexpected departed connection: %+v, %+v, %v", got, assessment, err)
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
		{airport: "SFO", arrivalGate: "A5", departureGate: "G9", arrivalTerminal: "I", departureTerminal: "I",
			instruction: "via gate areas B, C, D, E, F", transfer: 35},
		{airport: "EWR", arrivalGate: "A16", departureGate: "A25", arrivalTerminal: "A", departureTerminal: "Terminal A",
			instruction: "within Terminal A", transfer: 35},
		{airport: "JFK", arrivalGate: "A9", departureGate: "B28", arrivalTerminal: "T4", departureTerminal: "Terminal 4",
			instruction: "main concourse junction", transfer: 35},
		{airport: "LAX", arrivalGate: "42", departureGate: "64A", arrivalTerminal: "4", departureTerminal: "6",
			instruction: "bus", transfer: 65},
		{airport: "ORD", arrivalGate: "B7", departureGate: "G17", instruction: "walkways", transfer: 50},
		{airport: "DEN", arrivalGate: "A45", departureGate: "C44", instruction: "Train to the Gates", transfer: 50},
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

func TestBuiltInConnectionRoutesUseGenericScreeningEstimate(t *testing.T) {
	now := time.Now()
	arrival := now.Add(time.Hour)
	departure := arrival.Add(64 * time.Minute)
	for _, tc := range []struct {
		airport, fromTerminal, toTerminal, fromGate, toGate, transport string
	}{
		{"EWR", "", "", "A16", "C71", "AirTrain"},
		{"EWR", "B", "B", "B42", "B55", "checkpoint"},
		{"JFK", "T4", "5", "A9", "508", "AirTrain"},
		{"SIN", "2", "4", "E12", "G15", "transit shuttle"},
	} {
		t.Run(tc.airport+"/"+tc.fromGate, func(t *testing.T) {
			in := feeds.Flight{Destination: tc.airport, ArrivalTerminal: tc.fromTerminal, ArrivalGate: tc.fromGate, ScheduledArrival: &arrival}
			out := feeds.Flight{Origin: tc.airport, DepartureTerminal: tc.toTerminal, DepartureGate: tc.toGate, ScheduledDeparture: &departure}
			got, _, err := (Server{}).assessConnection(t.Context(), in, out, now, func(context.Context, feeds.Flight, bool, time.Time) (*core.GatePrediction, error) {
				return nil, nil
			})
			instructions := strings.Join(got.Instructions, " ")
			if err != nil || got.Precision != "generic" || got.TransferMinutes != defaultConnectionTerminalTransferMinutes ||
				got.BoardingReserveMinutes != defaultConnectionBoardingReserveMinutes || got.Risk != "Risky" || got.GuideURL == "" ||
				!strings.Contains(instructions, tc.transport) || strings.Contains(instructions, "stay in the secure area from your arrival gate") {
				t.Fatalf("unexpected public route %+v, error %v", got, err)
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
	for _, tc := range []struct {
		airport, icao, fromGate, toGate, fromTerminal, toTerminal, mapURL, guideURL, instruction string
		inOrigin, inOriginICAO, outDestination, outDestinationICAO                               string
		transfer, available                                                                      int
	}{
		{airport: "SEA", icao: "KSEA", fromGate: "N12", toGate: "S4", mapURL: "https://www.portseattle.org/sea/maps",
			guideURL: "https://www.portseattle.org/blog/all-aboard-sea-underground", instruction: "Yellow Line", transfer: 50},
		{airport: "EWR", icao: "KEWR", fromGate: "A16", toGate: "C71", mapURL: "https://www.newarkairport.com/explore-ewr/airport-map",
			guideURL: "https://www.newarkairport.com/flights/connections-guide", instruction: "covered walk", transfer: 65},
		{airport: "JFK", icao: "KJFK", fromGate: "A9", toGate: "508", mapURL: "https://www.jfkairport.com/explore-jfk/airport-map",
			guideURL: "https://www.jfkairport.com/flights/connections-guide", instruction: "free AirTrain", transfer: 65},
		{airport: "ORD", icao: "KORD", fromGate: "B7", toGate: "M13", mapURL: "https://maps.ord.flychicago.com/",
			guideURL: "https://www.flychicago.com/ohare/tofrom/connecting/Pages/default.aspx", instruction: "Airport Transit System", transfer: 65},
		{airport: "DEN", icao: "KDEN", fromGate: "A45", toGate: "C44", mapURL: "https://maps.flydenver.com/",
			guideURL: "https://www.flydenver.com/about-den/facilities/#train", instruction: "Train to the Gates", transfer: 50},
		{airport: "LHR", icao: "EGLL", fromTerminal: "Terminal 5", toTerminal: "Terminal 5", mapURL: "https://www.heathrow.com/at-the-airport/airport-maps",
			guideURL: "https://www.heathrow.com/connecting-flights", instruction: "security", transfer: 65},
		{airport: "YYZ", icao: "CYYZ", fromGate: "D38", toGate: "A7", fromTerminal: "Terminal 1", toTerminal: "Terminal 3",
			inOrigin: "YVR", inOriginICAO: "CYVR", outDestination: "LGA", outDestinationICAO: "KLGA",
			mapURL: "https://www.torontopearson.com/en/while-you-are-here/terminal-maps", guideURL: "https://www.torontopearson.com/en/connections",
			instruction: "Terminal Link train from Terminal 1 to Terminal 3", transfer: 65},
		{airport: "HND", icao: "RJTT", fromGate: "112", toGate: "60", fromTerminal: "3", toTerminal: "Terminal 2",
			inOrigin: "LAX", inOriginICAO: "KLAX", outDestination: "OKA", outDestinationICAO: "ROAH",
			mapURL: "https://tokyo-haneda.com/en/floor/index.html", guideURL: "https://tokyo-haneda.com/en/flight/transit/index.html",
			instruction: "Complete Japanese immigration", transfer: 65},
		{airport: "DXB", icao: "OMDB", fromGate: "A6", toGate: "F5", fromTerminal: "3", toTerminal: "Terminal 2",
			inOrigin: "LHR", inOriginICAO: "EGLL", outDestination: "MCT", outDestinationICAO: "OOMS",
			mapURL: "https://dubaiairports.ae/express-maps", guideURL: "https://dubaiairports.ae/information/transfers",
			instruction: "airside connections bus to Terminal 2", transfer: 105},
		{airport: "DOH", icao: "OTHH", fromGate: "B5", toGate: "E7", fromTerminal: "1", toTerminal: "Terminal 1",
			inOrigin: "LHR", inOriginICAO: "EGLL", outDestination: "MCT", outDestinationICAO: "OOMS",
			mapURL: "https://www.jal.co.jp/jp/en/inter/airport/doh/info/", guideURL: "https://www.jal.co.jp/jp/en/inter/airport/doh/transit/",
			instruction: "Passenger Train", transfer: 65},
		{airport: "DOH", icao: "OTHH", fromTerminal: "1", toTerminal: "1",
			inOrigin: "LHR", inOriginICAO: "EGLL", outDestination: "MCT", outDestinationICAO: "OOMS",
			mapURL: "https://www.jal.co.jp/jp/en/inter/airport/doh/info/", guideURL: "https://www.jal.co.jp/jp/en/inter/airport/doh/transit/",
			instruction: "transfer security", transfer: 65, available: 75},
	} {
		t.Run(tc.airport+"/"+tc.instruction, func(t *testing.T) {
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
			available := cmp.Or(tc.available, 64)
			depart := arrive.Add(time.Duration(available) * time.Minute)
			for _, flight := range []feeds.Flight{
				{ID: "inbound", Number: "PL101", Origin: cmp.Or(tc.inOrigin, "SFO"), OriginICAO: tc.inOriginICAO, Destination: tc.airport, DestinationICAO: tc.icao, ArrivalGate: tc.fromGate,
					ArrivalTerminal: tc.fromTerminal, ScheduledArrival: &arrive, ObservedAt: now, Status: "scheduled"},
				{ID: "outbound", Number: "PL202", Origin: tc.airport, OriginICAO: tc.icao, Destination: cmp.Or(tc.outDestination, "BOS"), DestinationICAO: tc.outDestinationICAO, DepartureGate: tc.toGate,
					DepartureTerminal: tc.toTerminal, ScheduledDeparture: &depart, ObservedAt: now, Status: "scheduled"},
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
			if got.Risk != "Risky" || got.AvailableMinutes != available || got.TransferMinutes != tc.transfer ||
				got.BoardingReserveMinutes != 15 || got.Precision != "generic" || got.MapURL != tc.mapURL || got.GuideURL != tc.guideURL ||
				!strings.Contains(strings.Join(got.Instructions, " "), tc.instruction) {
				t.Fatalf("unexpected tracked connection response: %+v", got)
			}
		})
	}
}
