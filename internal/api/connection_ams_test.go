package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestAMSConnectionDirectionsUsePublishedGateAreas(t *testing.T) {
	for _, tc := range []struct {
		name, fromGate, toGate, toArea string
		border                         bool
	}{
		{"Schengen piers", "B12", "C8", "Schengen", false},
		{"lower Schengen D gate", "C8", " gate d 59 ", "Schengen", false},
		{"upper Schengen D gate", "B12", "D87", "Schengen", false},
		{"non-Schengen gates", "D1", "E18", "non-Schengen", false},
		{"non-Schengen piers", "F5", "G8", "non-Schengen", false},
		{"enter Schengen within Pier D", "D57", "D59", "Schengen", true},
		{"leave Schengen", "D87", "H7", "non-Schengen", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalGate: tc.fromGate}
			out := feeds.Flight{DepartureGate: tc.toGate}
			got := amsConnectionDirections(in, out)
			if got.requiresBorderControl != tc.border || got.landside || got.requiresScreening || got.secureSameTerminal {
				t.Fatalf("directions = %+v, want only border control = %t", got, tc.border)
			}
			instructions := strings.Join(got.steps, " ")
			if strings.Contains(instructions, "passport control") != tc.border {
				t.Errorf("directions = %q, want passport control instructions = %t", got.steps, tc.border)
			}
			for _, want := range []string{
				"yellow transfer signs", "the " + tc.toArea + " area", "if screening is required",
				"airport screens for your current departure gate",
				"If you need to collect and recheck checked bags",
			} {
				if !strings.Contains(instructions, want) {
					t.Errorf("directions = %q, missing %q", got.steps, want)
				}
			}
		})
	}
}

func TestAMSConnectionDirectionsRequireKnownGates(t *testing.T) {
	for _, tc := range []struct{ name, terminal, gate string }{
		{"missing gate", "", ""},
		{"departure hall only", "Departures 1", ""},
		{"ambiguous D gate", "", "D58"},
		{"outside published D range", "", "D88"},
		{"ambiguous M pier", "", "M3"},
		{"pier without gate", "", "B"},
		{"invalid gate", "", "B5/B6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.terminal, ArrivalGate: tc.gate}
			out := feeds.Flight{DepartureGate: "E18"}
			if got := amsConnectionDirections(in, out); len(got.steps) != 0 || got.requiresBorderControl {
				t.Fatalf("unexpected arrival directions %+v", got)
			}
			in = feeds.Flight{ArrivalGate: "B12"}
			out = feeds.Flight{DepartureTerminal: tc.terminal, DepartureGate: tc.gate}
			if got := amsConnectionDirections(in, out); len(got.steps) != 0 || got.requiresBorderControl {
				t.Fatalf("unexpected departure directions %+v", got)
			}
		})
	}
}
