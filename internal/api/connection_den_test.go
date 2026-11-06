package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestDENConnectionDirectionsUseGateConcourses(t *testing.T) {
	for _, tc := range []struct {
		name, arrivalTerminal, arrivalGate, departureTerminal, departureGate, want string
		sameConcourse, entryGuidance                                               bool
	}{
		{"secure train and conditional entry", "Jeppesen Terminal", "Gate A19W", "C Gates", "C67", "center of the A gates. Take the train to the C gates", false, true},
		{"reverse train", "", "C66", "Concourse B", "B07", "center of the C gates. Take the train to the B gates", false, false},
		{"same concourse", "Terminal West", "B7", "", "B95", "within the B gates", true, false},
		{"reported concourse", "Gates A", "", "Concourse A", "", "within the A gates", true, true},
		{"contradictory concourse", "B", "A53", "C", "C67", "", false, false},
		{"unknown gate prefix", "A", "D55", "C", "C67", "", false, false},
		{"out of range gate", "A", "A99", "C", "C67", "", false, false},
		{"missing concourse", "Jeppesen Terminal", "", "C", "C67", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.arrivalTerminal, ArrivalGate: tc.arrivalGate}
			out := feeds.Flight{DepartureTerminal: tc.departureTerminal, DepartureGate: tc.departureGate}
			route := denConnectionDirections(in, out)
			if tc.want == "" {
				if len(route.steps) != 0 || route.secureSameTerminal || route.landside {
					t.Fatalf("unverified endpoints received directions: %+v", route)
				}
				return
			}
			if len(route.steps) == 0 || !strings.Contains(route.steps[0], tc.want) || route.secureSameTerminal != tc.sameConcourse || route.landside {
				t.Fatalf("directions = %+v, want %q", route, tc.want)
			}
			if strings.Contains(strings.Join(route.steps, " "), "If you still need U.S. entry processing") != tc.entryGuidance {
				t.Fatalf("unexpected entry guidance: %+v", route)
			}
		})
	}
}
