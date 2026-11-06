package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestHNDConnectionDirections(t *testing.T) {
	tests := []struct {
		name, from, fromGate, origin, to, toGate, destination string
		want                                                  []string
		border, sameSecure                                    bool
	}{
		{
			name: "domestic same Terminal 1", from: "1", to: "Terminal 1", sameSecure: true,
			want: []string{"Domestic Connecting Flights", "arrival concourse"},
		},
		{
			name: "domestic same Terminal 2 with swing gate", from: "t 2", fromGate: "Gate 67A", origin: "roah", to: "2", toGate: "65", sameSecure: true,
			want: []string{"Domestic Connecting Flights", "arrival concourse"},
		},
		{
			name: "domestic Terminal 1 to Terminal 2", from: "T1", to: "2", toGate: "500",
			want: []string{"Bus Stop 8", "Terminal 2", "domestic security"},
		},
		{
			name: "domestic Terminal 2 to Terminal 1", from: "2", origin: "RJCC", to: "1",
			want: []string{"Bus Stop 9", "Terminal 1", "domestic security"},
		},
		{
			name: "domestic to international Terminal 2", from: "2", fromGate: "49", to: "2", destination: "VHHH",
			want: []string{"International Flight Transfer", "first floor", "third floor", "departure passport control"},
		},
		{
			name: "domestic Terminal 1 to international Terminal 2", from: "1", to: "2", toGate: "72",
			want: []string{"Bus Stop 8", "Terminal 2", "departure passport control"},
		},
		{
			name: "domestic Terminal 1 to international Terminal 3", from: "1", to: "3",
			want: []string{"If your airline confirms", "International Transit", "dedicated bus to Terminal 3", "Bus Stop 8", "departure passport control"},
		},
		{
			name: "domestic Terminal 2 to international Terminal 3", from: "2", origin: "RJFF", to: "3",
			want: []string{"If your airline confirms", "International Flight Transfer", "dedicated bus to Terminal 3", "Bus Stop 9"},
		},
		{
			name: "international to domestic Terminal 2", from: "2", origin: "EGLL", to: "2", destination: "RJFF", border: true,
			want: []string{"Complete Japanese immigration", "collect any checked bags", "customs", "second floor of Terminal 2", "domestic security"},
		},
		{
			name: "international Terminal 2 to domestic Terminal 1", from: "2", origin: "EGLL", to: "1", border: true,
			want: []string{"Complete Japanese immigration", "Bus Stop 9", "Terminal 1", "domestic security"},
		},
		{
			name: "international Terminal 3 to domestic Terminal 1", from: "3", to: "1", border: true,
			want: []string{"Complete Japanese immigration", "domestic transfer security checkpoint", "transfer bus to Terminal 1", "Bus Stop 0"},
		},
		{
			name: "international Terminal 3 to domestic Terminal 2", from: "3", fromGate: "112", to: "Terminal 2", toGate: "60", destination: "ROAH", border: true,
			want: []string{"Complete Japanese immigration", "domestic transfer security checkpoint", "transfer bus to Terminal 2", "Bus Stop 0"},
		},
		{
			name: "international same Terminal 2", from: "2", fromGate: "70", to: "2", toGate: "700",
			want: []string{"stay in transit", "International Transit", "within Terminal 2", "international transit security", "confirm your eligibility to enter"},
		},
		{
			name: "international same Terminal 3", from: "3", to: "3",
			want: []string{"stay in transit", "International Transit", "within Terminal 3", "international transit security"},
		},
		{
			name: "international Terminal 2 to Terminal 3", from: "2", origin: "KLAX", to: "3",
			want: []string{"stay in transit", "dedicated bus to Terminal 3", "international transit security", "confirm your eligibility to enter", "Bus Stop 9"},
		},
		{
			name: "international Terminal 3 to Terminal 2", from: "3", to: "2", destination: "RKSI",
			want: []string{"stay in transit", "dedicated bus to Terminal 2", "international transit security", "confirm your eligibility to enter", "Bus Stop 0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tt.from, ArrivalGate: tt.fromGate, OriginICAO: tt.origin}
			out := feeds.Flight{DepartureTerminal: tt.to, DepartureGate: tt.toGate, DestinationICAO: tt.destination}
			got := hndConnectionDirections(in, out)
			if got.requiresBorderControl != tt.border || got.secureSameTerminal != tt.sameSecure ||
				got.requiresScreening == tt.sameSecure || got.transferMinutes != 0 {
				t.Fatalf("unexpected directions flags: %+v", got)
			}
			remaining := strings.Join(got.steps, " ")
			for _, want := range tt.want {
				_, after, found := strings.Cut(remaining, want)
				if !found {
					t.Fatalf("expected %q in order in %v", want, got.steps)
				}
				remaining = after
			}
			instructions := strings.Join(got.steps, " ")
			if strings.Contains(instructions, "walkway") || strings.Contains(instructions, "Complete Japanese immigration") != tt.border {
				t.Fatalf("unexpected walkthrough: %v", got.steps)
			}
		})
	}
}

func TestHNDConnectionDirectionsWithoutReliableAssignment(t *testing.T) {
	tests := []struct {
		name, terminal, gate, origin string
	}{
		{name: "missing terminal", gate: "60", origin: "RJFF"},
		{name: "unknown terminal", terminal: "4", gate: "60", origin: "RJFF"},
		{name: "mixed terminal without geography", terminal: "2"},
		{name: "mixed terminal swing gate", terminal: "2", gate: "69"},
		{name: "unknown location indicator", terminal: "2", origin: "ZZZZ"},
		{name: "not an ICAO indicator", terminal: "2", origin: "HKG"},
		{name: "reported terminal conflicts with gate", terminal: "2", gate: "112", origin: "KLAX"},
		{name: "reported terminal conflicts with geography", terminal: "1", origin: "KLAX"},
		{name: "international terminal conflicts with geography", terminal: "3", origin: "ROAH"},
		{name: "mixed terminal domestic gate conflicts with geography", terminal: "2", gate: "60", origin: "KLAX"},
		{name: "mixed terminal international gate conflicts with geography", terminal: "2", gate: "70", origin: "ROAH"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tt.terminal, ArrivalGate: tt.gate, OriginICAO: tt.origin}
			out := feeds.Flight{DepartureTerminal: "3", DestinationICAO: "KLAX"}
			got := hndConnectionDirections(in, out)
			if len(got.steps) != 0 || got.requiresScreening || got.requiresBorderControl || got.secureSameTerminal || got.transferMinutes != 0 {
				t.Fatalf("unexpected directions without reliable assignment: %+v", got)
			}
		})
	}
}
