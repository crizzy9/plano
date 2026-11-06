package api

import (
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestFRAConnectionDirections(t *testing.T) {
	for _, tc := range []struct {
		name, fromTerminal, fromGate, toTerminal, toGate, instruction string
		transferMinutes                                               int
		border, samePier, shuttle                                     bool
	}{
		{"terminal change with border", "Terminal 1", "A15", "T3", "J7", "SkyLine signs from Terminal 1 to Terminal 3", 65, true, false, true},
		{"terminal change from gate families", "", "GATE H 4", "", "A16", "SkyLine signs from Terminal 3 to Terminal 1", 65, false, false, true},
		{"terminal change without gates", "terminal3", "", "T 1", "", "SkyLine signs from Terminal 3 to Terminal 1", 65, false, false, true},
		{"Terminal 1 tunnel", "1", "B19", "", "A15", "in the Schengen area, use the passenger tunnel between A15 and B19", 0, false, false, false},
		{"Terminal 1 border", "1", "Z22", "1", "A15", "Follow transfer signs within Terminal 1", 0, true, false, false},
		{"Terminal 3 border", "3", "H13", "3", "J18", "Follow transfer signs within Terminal 3", 0, true, false, false},
		{"mixed pier does not determine border", "3", "G3", "3", "J18", "Follow transfer signs within Terminal 3", 0, false, false, false},
		{"same pier", "terminal 1", "A15", "T1", "A17", "Follow transfer signs within Terminal 1", 0, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.fromTerminal, ArrivalGate: tc.fromGate}
			out := feeds.Flight{DepartureTerminal: tc.toTerminal, DepartureGate: tc.toGate}
			got := fraConnectionDirections(in, out)
			steps := strings.Join(got.steps, " ")
			if got.transferMinutes != tc.transferMinutes || got.requiresBorderControl != tc.border || got.secureSameTerminal != tc.samePier ||
				got.requiresScreening || got.landside || strings.Contains(steps, "Clear passport control") != tc.border ||
				strings.Contains(steps, "shuttle stops outside Arrivals") != tc.shuttle {
				t.Fatalf("unexpected route %+v", got)
			}
			for _, text := range []string{tc.instruction, "onward gate", "onward boarding pass", "required transfer security", "collect bags or check in again"} {
				if !strings.Contains(steps, text) {
					t.Errorf("instructions %q missing %q", steps, text)
				}
			}
		})
	}
}

func TestFRAConnectionDirectionsFallsBackWithoutUsableEndpoints(t *testing.T) {
	beforeOpening := fraTerminal3Opened.Add(-time.Minute)
	for _, tc := range []struct {
		name, fromTerminal, fromGate string
		arrival                      *time.Time
	}{
		{name: "closed Terminal 2", fromTerminal: "2", fromGate: "J7"},
		{name: "old Terminal 2 gate", fromGate: "E12"},
		{name: "unrecognized terminal", fromTerminal: "4", fromGate: "H1"},
		{name: "terminal and gate conflict", fromTerminal: "3", fromGate: "Z11"},
		{name: "unrecognized gate", fromTerminal: "1", fromGate: "X17"},
		{name: "missing endpoint"},
		{name: "Terminal 3 before opening", fromTerminal: "3", fromGate: "J7", arrival: &beforeOpening},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.fromTerminal, ArrivalGate: tc.fromGate, ScheduledArrival: tc.arrival}
			out := feeds.Flight{DepartureTerminal: "1", DepartureGate: "A15"}
			if got := fraConnectionDirections(in, out); len(got.steps) != 0 || got.transferMinutes != 0 || got.requiresBorderControl {
				t.Fatalf("unsupported endpoint received directions %+v", got)
			}
		})
	}
}
