package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestLHRConnectionDirectionsAlwaysRequireScreening(t *testing.T) {
	for _, tc := range []struct {
		name, arrivalTerminal, departureTerminal string
		want, avoid                              string
	}{
		{"different terminals", " terminal 2 ", "T5", "free Flight Connections shuttle bus from Terminal 2 to Terminal 5", "connections security in Terminal 5"},
		{"same terminal", "5", "Terminal 5", "connections security in Terminal 5", "shuttle bus"},
		{"missing terminals with gates", "", "", "Check the connection screens for your onward terminal", "from Terminal"},
		{"unrecognized arrival terminal", "Terminal 1", "3", "if you need to change terminals", "from Terminal"},
		{"unrecognized departure terminal", "T4", "Terminal 6", "Check the connection screens for your onward terminal", "from Terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.arrivalTerminal, ArrivalGate: "A12"}
			out := feeds.Flight{DepartureTerminal: tc.departureTerminal, DepartureGate: "A20"}
			got := lhrConnectionDirections(in, out)
			if !got.requiresScreening || got.landside || got.secureSameTerminal {
				t.Fatalf("incorrect Heathrow route flags: %+v", got)
			}
			directions := strings.Join(got.steps, " ")
			for _, want := range []string{tc.want, "same ticket", "purple Flight Connections signs", "biometrics before security", "separate tickets", "required immigration", "checked bags", "check in", "All passengers must clear security"} {
				if !strings.Contains(directions, want) {
					t.Errorf("directions = %q, missing %q", got.steps, want)
				}
			}
			if strings.Contains(directions, tc.avoid) {
				t.Errorf("directions = %q, unexpectedly contain %q", got.steps, tc.avoid)
			}
		})
	}
}
