package api

import (
	"slices"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestLAXConnectionDirectionsUseCurrentConnectors(t *testing.T) {
	for _, tc := range []struct {
		name, fromTerminal, fromGate, toTerminal, toGate string
		want                                             []string
	}{
		{
			name: "north to south across Terminal 5 closure", fromTerminal: "T1", fromGate: " 12a ", toTerminal: "8", toGate: "84",
			want: []string{
				"Follow signed secure walkways from Terminal 1 to Terminal 4 via the Terminal 2 gate area, Terminal 3, Terminal B.",
				"Follow signs for the secure airfield bus from Terminal 4 to Terminal 6. Confirm the current boarding location with airport staff.",
				"Follow signed secure walkways from Terminal 6 to Terminal 8 via Terminal 7.",
			},
		},
		{
			name: "south to West Gates", fromTerminal: "Terminal 6", fromGate: "62", toTerminal: "tbit", toGate: "233",
			want: []string{
				"Follow signs for the secure airfield bus from Terminal 6 to Terminal 4. Confirm the current boarding location with airport staff.",
				"Follow signed secure walkways from Terminal 4 to Terminal B.",
				"Follow signs through the Gateway passenger tunnel from Terminal B to the West Gates.",
			},
		},
		{
			name: "West Gates to Terminal B", fromTerminal: "West Gates", fromGate: "209B", toTerminal: "Terminal B", toGate: "152",
			want: []string{"Follow signs through the Gateway passenger tunnel from the West Gates to Terminal B."},
		},
		{
			name: "same concourse", fromTerminal: "Terminal 7", fromGate: "70B", toGate: "Gate 75A",
			want: []string{"Follow departure gate signs within Terminal 7."},
		},
		{
			name: "Terminal 2 gate in shared complex", fromTerminal: "Terminal 3", fromGate: "23A", toTerminal: "3", toGate: "31A",
			want: []string{"Follow signed secure walkways from the Terminal 2 gate area to Terminal 3."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.fromTerminal, ArrivalGate: tc.fromGate}
			out := feeds.Flight{DepartureTerminal: tc.toTerminal, DepartureGate: tc.toGate}
			if got := laxConnectionDirections(in, out); !slices.Equal(got, tc.want) {
				t.Fatalf("directions = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLAXConnectionDirectionsRequireCompatibleReportedGates(t *testing.T) {
	for _, tc := range []struct{ name, terminal, gate string }{
		{"missing gate", "4", ""},
		{"malformed gate", "4", "41-42"},
		{"unknown gate range", "B", "200"},
		{"unknown terminal label", "East", "152"},
		{"West label on main Terminal B gate", "West", "152"},
		{"closed Terminal 5", "5", "55"},
		{"remote gate with Terminal 4 check-in", "4", "52A"},
		{"Terminal 1 check-in with West Gate", "1", "213"},
		{"Terminal 2 label on Terminal 3 gate", "2", "31A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.terminal, ArrivalGate: tc.gate}
			out := feeds.Flight{DepartureTerminal: "7", DepartureGate: "74"}
			if got := laxConnectionDirections(in, out); got != nil {
				t.Fatalf("unexpected directions %q", got)
			}
			in = feeds.Flight{ArrivalTerminal: "7", ArrivalGate: "74"}
			out = feeds.Flight{DepartureTerminal: tc.terminal, DepartureGate: tc.gate}
			if got := laxConnectionDirections(in, out); got != nil {
				t.Fatalf("unexpected reverse directions %q", got)
			}
		})
	}
}
