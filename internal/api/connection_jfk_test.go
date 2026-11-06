package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestJFKConnectionDirectionsUsePublishedRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, arrivalTerminal, arrivalGate, departureTerminal, departureGate string
		landside                                                             bool
		want                                                                 []string
	}{
		{
			name: "public transfer between reported terminals", arrivalTerminal: "Terminal 1", departureTerminal: "T8", departureGate: "31",
			landside: true, want: []string{"from Terminal 1", "free AirTrain to Terminal 8", "replacement service", "TSA screening in Terminal 8"},
		},
		{
			name: "public transfer to inferred Terminal 5", arrivalTerminal: "7", arrivalGate: "11A", departureGate: "Gate 501",
			landside: true, want: []string{"from Terminal 7", "free AirTrain to Terminal 5", "TSA screening in Terminal 5"},
		},
		{
			name: "public transfer from inferred Terminal 5", arrivalGate: "530", departureTerminal: "7", departureGate: "12",
			landside: true, want: []string{"from Terminal 5", "free AirTrain to Terminal 7", "TSA screening in Terminal 7"},
		},
		{
			name: "Terminal 4 junction to B", arrivalGate: " a3 ", departureTerminal: "4", departureGate: "B55",
			want: []string{"main concourse junction", "from Concourse A to Concourse B in Terminal 4"},
		},
		{
			name: "Terminal 4 junction to A", arrivalGate: "B18", departureGate: "A21",
			want: []string{"main concourse junction", "from Concourse B to Concourse A in Terminal 4"},
		},
		{
			name: "Terminal 8 tunnel to C", arrivalTerminal: "8", arrivalGate: "20", departureTerminal: "8", departureGate: "31",
			want: []string{"underground walkway", "from Concourse B to Concourse C in Terminal 8"},
		},
		{
			name: "Terminal 8 tunnel to B", arrivalTerminal: "Terminal 8", arrivalGate: "47", departureTerminal: "T8", departureGate: "1",
			want: []string{"underground walkway", "from Concourse C to Concourse B in Terminal 8"},
		},
		{
			name: "same terminal with no reported gates", arrivalTerminal: "T4", departureTerminal: "Terminal 4",
			want: []string{"Follow departure gate signs within Terminal 4."},
		},
		{
			name: "same Terminal 1", arrivalTerminal: "1", arrivalGate: "1", departureTerminal: "T1", departureGate: "11",
			want: []string{"Follow departure gate signs within Terminal 1."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.arrivalTerminal, ArrivalGate: tc.arrivalGate}
			out := feeds.Flight{DepartureTerminal: tc.departureTerminal, DepartureGate: tc.departureGate}
			got := jfkConnectionDirections(in, out)
			if got.landside != tc.landside {
				t.Fatalf("landside = %v, want %v, directions = %q", got.landside, tc.landside, got.steps)
			}
			directions := strings.Join(got.steps, " ")
			for _, want := range tc.want {
				if !strings.Contains(directions, want) {
					t.Errorf("directions = %q, missing %q", got.steps, want)
				}
			}
		})
	}
}

func TestJFKConnectionDirectionsRequireCompatibleReportedEndpoints(t *testing.T) {
	for _, tc := range []struct{ name, terminal, gate string }{
		{"missing terminal and gate", "", ""},
		{"ambiguous low gate", "", "10"},
		{"numeric Terminal 8 gate with no reported terminal", "", "42"},
		{"terminal conflicts with inferred Terminal 4", "5", "B30"},
		{"terminal conflicts with inferred Terminal 5", "4", "510"},
		{"unknown terminal", "East", "A10"},
		{"incomplete terminal label", "T", "A10"},
		{"unopened Terminal 6", "6", ""},
		{"unopened terminal with gate", "T6", "505"},
		{"closed Terminal 2", "2", ""},
		{"unrecognized Terminal 4 gate", "4", "B17"},
		{"unrecognized Terminal 8 gate", "8", "11"},
		{"invalid gate shape", "4", "B30-B32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.terminal, ArrivalGate: tc.gate}
			out := feeds.Flight{DepartureTerminal: "8", DepartureGate: "42"}
			if got := jfkConnectionDirections(in, out); len(got.steps) != 0 || got.landside {
				t.Fatalf("unexpected arrival directions %+v", got)
			}
			in = feeds.Flight{ArrivalTerminal: "8", ArrivalGate: "42"}
			out = feeds.Flight{DepartureTerminal: tc.terminal, DepartureGate: tc.gate}
			if got := jfkConnectionDirections(in, out); len(got.steps) != 0 || got.landside {
				t.Fatalf("unexpected departure directions %+v", got)
			}
		})
	}
}
