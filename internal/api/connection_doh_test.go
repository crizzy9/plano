package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestDOHConnectionDirections(t *testing.T) {
	for _, tc := range []struct {
		name, fromTerminal, fromGate, toTerminal, toGate, direction string
		train                                                       bool
	}{
		{"same concourse still needs screening", "1", "A1", "Main Terminal", "A11", "Concourse A", false},
		{"normalizes gates and terminal", "t 1", "C93", "Terminal 1", " gate b 10 ", "Concourse B", false},
		{"train may serve Concourse C", "", "B2", "", "C80", "Concourse C", true},
		{"train may serve Concourse D", "", "E20", "", "D9", "Concourse D", true},
		{"train may serve Concourse E", "", "D24", "", "E8", "Concourse E", true},
		{"level zero gate", "", "C8", "", "E21", "Concourse E", true},
		{"missing arrival gate with terminal", "T1", "", "", "D2", "Concourse D", true},
		{"missing departure gate with terminal", "", "B2", "1", "", "confirmed departure gate", false},
		{"main terminal without gates", "T1", "", "1", "", "confirmed departure gate", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.fromTerminal, ArrivalGate: tc.fromGate}
			out := feeds.Flight{DepartureTerminal: tc.toTerminal, DepartureGate: tc.toGate}
			got := dohConnectionDirections(in, out)
			steps := strings.Join(got.steps, " ")
			if !got.requiresScreening || got.requiresBorderControl || got.landside || got.secureSameTerminal || got.transferMinutes != 0 {
				t.Fatalf("unexpected directions flags: %+v", got)
			}
			if strings.Contains(steps, "Passenger Train") != tc.train {
				t.Fatalf("unexpected Passenger Train instructions: %v", got.steps)
			}
			remaining := steps
			for _, want := range []string{"stay in transit", "transfer security", "departure screens", tc.direction, "eligibility to enter Qatar"} {
				_, after, found := strings.Cut(remaining, want)
				if !found {
					t.Fatalf("expected %q in order in %v", want, got.steps)
				}
				remaining = after
			}
		})
	}
}

func TestDOHConnectionDirectionsWithoutUsableEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name, fromTerminal, fromGate, toTerminal, toGate string
	}{
		{"missing arrival location", "", "", "1", "A1"},
		{"missing departure location", "1", "A1", "", ""},
		{"unsupported departure terminal", "1", "A1", "2", "E1"},
		{"unsupported arrival concourse", "", "F1", "", "A1"},
		{"unsupported arrival gate number", "", "B11", "", "A1"},
		{"unsupported Concourse C gate number", "1", "A1", "1", "C44"},
		{"unsupported departure gate number", "", "A1", "", "E12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.fromTerminal, ArrivalGate: tc.fromGate}
			out := feeds.Flight{DepartureTerminal: tc.toTerminal, DepartureGate: tc.toGate}
			if got := dohConnectionDirections(in, out); len(got.steps) != 0 || got.requiresScreening || got.transferMinutes != 0 {
				t.Fatalf("unexpected directions without usable endpoints: %+v", got)
			}
		})
	}
}
