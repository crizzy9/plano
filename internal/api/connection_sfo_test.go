package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestSFOConnectionDirectionsUseReportedGateAreas(t *testing.T) {
	for _, tc := range []struct {
		name, fromTerminal, fromGate, toTerminal, toGate, want string
	}{
		{"distinct international gate areas", "International Terminal", "A15", "I", "G14", "A gates via gate areas B, C, D, E, F to the G gates"},
		{"reverse secure walkway", "G", "G1", "A", "A1", "G gates via gate areas F, E, D, C, B to the A gates"},
		{"same gate area", "Harvey Milk Terminal 1", "Gate B02", "T1", "B15", "within the B gates"},
		{"adjacent areas", "Terminal 1", "C4", "Terminal 2", "D2", "from the C gates to the D gates"},
		{"terminal conflicts with gate", "2", "E2", "3", "F5", ""},
		{"international area conflict", "A", "G1", "I", "A1", ""},
		{"out-of-range gate", "3", "E1", "3", "F5", ""},
		{"unused Terminal 2 gate number", "2", "D13", "3", "F5", ""},
		{"missing gate", "1", "", "3", "E2", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{Destination: "SFO", DestinationICAO: "KSFO", ArrivalGate: tc.fromGate, ArrivalTerminal: tc.fromTerminal}
			out := feeds.Flight{Origin: "SFO", OriginICAO: "KSFO", DepartureGate: tc.toGate, DepartureTerminal: tc.toTerminal}
			route, configured := (*ConnectionRules)(nil).routeForConnection(in, out)
			if configured || route.MapURL != "https://www.flysfo.com/maps" {
				t.Fatalf("unexpected route source: %+v", route)
			}
			if tc.want == "" {
				if route.GuideURL != "" || len(route.Instructions) != 0 {
					t.Fatalf("ambiguous gate received directions: %+v", route)
				}
				return
			}
			if route.GuideURL != "https://www.flysfo.com/connecting-flights-faq" || len(route.Instructions) != 2 ||
				!strings.Contains(route.Instructions[0], "if you can stay in the secure area") || !strings.Contains(route.Instructions[1], tc.want) {
				t.Fatalf("SFO directions = %+v, want %q", route, tc.want)
			}
		})
	}
}
