package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestDXBConnectionDirections(t *testing.T) {
	tests := []struct {
		name, fromTerminal, fromGate, toTerminal, toGate string
		want                                             []string
		transferMinutes                                  int
	}{
		{
			name: "A to B train", fromTerminal: " terminal 3 ", fromGate: "Gate A 06", toGate: "B21",
			want: []string{"Terminal 3 airport train", "B gates"},
		},
		{
			name: "B to C walkway", fromGate: "B21", toTerminal: "T 3", toGate: "C12A",
			want: []string{"Terminal 3 walkway", "C gates"},
		},
		{
			name: "C to A train and shuttle options", fromGate: "C12", toGate: "A6",
			want: []string{"airport train serving A gates", "walkway between B and C gates", "if the Emirates shuttle between A and C serves your connection"},
		},
		{
			name: "Terminal 3 to 2 connections bus", fromGate: "A6", toTerminal: "2", toGate: "F5",
			want: []string{"Ask the transfer desk if you can take the airside connections bus to Terminal 2"}, transferMinutes: 105,
		},
		{
			name: "Terminal 2 to 3 connections bus", fromGate: "F5", toTerminal: "Terminal 3",
			want: []string{"Ask the transfer desk if you can take the airside connections bus to Terminal 3"}, transferMinutes: 105,
		},
		{
			name: "Terminal 3 to 1 airline shuttle", fromGate: "B21", toGate: "D10",
			want: []string{"if your airline can direct you to the airside shuttle for Terminal 1 D gates"},
		},
		{
			name: "Terminal 1 to 3 staff advice", fromGate: "D10", toGate: "B21",
			want: []string{"which transfer route is available to Terminal 3 before leaving the secure area"},
		},
		{
			name: "Terminal 1 to 2 staff advice", fromGate: "D10", toGate: "F5",
			want: []string{"which transfer route is available to Terminal 2 before leaving the secure area"},
		},
		{
			name: "same Terminal 1", fromGate: "D10", toGate: "D12",
			want: []string{"departure signs within Terminal 1"},
		},
		{
			name: "Terminal 3 with one unknown concourse", fromTerminal: "3", toGate: "A6",
			want: []string{"departure signs within Terminal 3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tt.fromTerminal, ArrivalGate: tt.fromGate}
			out := feeds.Flight{DepartureTerminal: tt.toTerminal, DepartureGate: tt.toGate}
			got := dxbConnectionDirections(in, out)
			if !got.requiresScreening || got.landside || got.requiresBorderControl || got.secureSameTerminal || got.transferMinutes != tt.transferMinutes {
				t.Fatalf("unexpected directions flags: %+v", got)
			}
			want := append([]string{"if your airline confirms you can remain in transit", "any required transfer security"}, tt.want...)
			want = append(want, "If you need to collect bags or check in again", "check your UAE entry requirements", "passport control", "departure security")
			remaining := strings.Join(got.steps, " ")
			for _, phrase := range want {
				_, after, found := strings.Cut(remaining, phrase)
				if !found {
					t.Fatalf("expected %q in order in %v", phrase, got.steps)
				}
				remaining = after
			}
		})
	}
}

func TestDXBConnectionDirectionsWithoutReliableAssignment(t *testing.T) {
	tests := []struct {
		name, terminal, gate string
	}{
		{name: "missing endpoint"},
		{name: "unsupported terminal", terminal: "4", gate: "A6"},
		{name: "malformed terminal", terminal: "Terminal", gate: "A6"},
		{name: "unsupported concourse", terminal: "3", gate: "E5"},
		{name: "unsupported gate format", terminal: "3", gate: "18"},
		{name: "terminal conflicts with concourse", terminal: "2", gate: "D10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			arrival := feeds.Flight{ArrivalTerminal: tt.terminal, ArrivalGate: tt.gate}
			departure := feeds.Flight{DepartureTerminal: tt.terminal, DepartureGate: tt.gate}
			for _, got := range []connectionGuideDirections{
				dxbConnectionDirections(arrival, feeds.Flight{DepartureGate: "A6"}),
				dxbConnectionDirections(feeds.Flight{ArrivalGate: "A6"}, departure),
			} {
				if len(got.steps) != 0 || got.requiresScreening || got.landside || got.requiresBorderControl || got.secureSameTerminal || got.transferMinutes != 0 {
					t.Fatalf("unexpected directions without reliable assignment: %+v", got)
				}
			}
		})
	}
}
