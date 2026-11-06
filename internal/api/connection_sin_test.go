package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestSINConnectionDirectionsUseVerifiedTerminalRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, arrivalTerminal, arrivalGate, departureTerminal, departureGate string
		route, security                                                      string
	}{
		{"same terminal", "Terminal 1", "C21", "t1", "D36", "within Terminal 1", "at your departure gate"},
		{"Skytrain or walking", "T 3", "Gate B05", "", "F51", "Follow transit signs to Terminal 2", "at your departure gate"},
		{"transit connection between T1 and T2", "1", "C21", "2", "E12", "Skytrain connection or airside walkway", "at your departure gate"},
		{"shuttle from T1", "", "C16", "T4", "G4", "Gate C21 in Terminal 1 to Terminal 4", "central security screening in Terminal 4"},
		{"shuttle from T2", "Terminal 2", "E12", "4", "", "Arrival Immigration Hall F in Terminal 2 to Terminal 4", "central security screening in Terminal 4"},
		{"shuttle from T3", "", "A15", "", "G15", "Arrival Immigration Hall A in Terminal 3 to Terminal 4", "central security screening in Terminal 4"},
		{"shuttle from T4", "T4", "G4", "", "B05", "the Transfer Lounge in Terminal 4 to Terminal 3", "at your departure gate"},
		{"same T4", "4", "G15", "T4", "G4", "within Terminal 4", "central security screening in Terminal 4"},
		{"contradictory terminal and gate", "T1", "A15", "T4", "G4", "", ""},
		{"unknown terminal", "T5", "D36", "T4", "G4", "", ""},
		{"unknown gate prefix", "T1", "H14", "T4", "G4", "", ""},
		{"missing terminal and gate", "", "", "T4", "G4", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.arrivalTerminal, ArrivalGate: tc.arrivalGate}
			out := feeds.Flight{DepartureTerminal: tc.departureTerminal, DepartureGate: tc.departureGate}
			got := sinConnectionDirections(in, out)
			if tc.route == "" {
				if len(got.steps) != 0 || got.requiresScreening {
					t.Fatalf("unverified endpoints received directions: %+v", got)
				}
				return
			}
			if !got.requiresScreening || got.landside || got.requiresBorderControl || got.transferMinutes != 0 {
				t.Fatalf("incorrect Changi route flags: %+v", got)
			}
			steps := strings.Join(got.steps, " ")
			for _, want := range []string{tc.route, tc.security, "one itinerary", "tagged to your final destination", "separate tickets", "entry requirements"} {
				if !strings.Contains(steps, want) {
					t.Errorf("directions %q missing %q", steps, want)
				}
			}
		})
	}
}
