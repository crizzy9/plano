package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestEWRConnectionDirectionsUseTerminalAccessAndSecurity(t *testing.T) {
	for _, tc := range []struct {
		name, fromTerminal, fromGate, toTerminal, toGate string
		landside                                         bool
		want, absent                                     []string
	}{
		{"depart from Terminal A", "Terminal A", "Gate A07", "C", "125", true,
			[]string{"before leaving the terminal", "If AirTrain is serving Terminal A", "from Terminal A to its AirTrain station", "free AirTrain toward Terminal C", "posted signs for replacement service", "Clear TSA in Terminal C"},
			[]string{"Terminal A departures"}},
		{"arrive at Terminal A", "", "C123", "", "A34", true,
			[]string{"free AirTrain toward Terminal A", "at the Terminal A AirTrain station", "shuttle to Terminal A departures", "Clear TSA in Terminal A"},
			[]string{"from Terminal A to its AirTrain station"}},
		{"depart from Terminal B checkpoint", "C", "C70", "B", "B68", true,
			[]string{"free AirTrain toward Terminal B", "checkpoint for gates B60–B68"},
			[]string{"covered walk"}},
		{"terminals known without gates", "C", "", "A", "", true,
			[]string{"free AirTrain toward Terminal A", "covered walk or signed public shuttle", "Clear TSA in Terminal A"}, nil},
		{"stay in Terminal A", "A", "A1", "A", "34", false,
			[]string{"within Terminal A"}, []string{"AirTrain", "Clear TSA"}},
		{"stay in Terminal C", "C", "C70", "C", "C139", false,
			[]string{"within Terminal C"}, []string{"AirTrain", "Clear TSA"}},
		{"stay in Terminal B checkpoint", "B", "B40", "B", "49", false,
			[]string{"within the B40–B49 gate area"}, []string{"AirTrain", "Clear TSA"}},
		{"change Terminal B checkpoints", "", "B49", "", "B51", true,
			[]string{"public departures hall in Terminal B", "Clear TSA at the checkpoint for gates B51–B57"}, []string{"AirTrain"}},
		{"reverse Terminal B checkpoints", "B", "B68", "B", "B40", true,
			[]string{"public departures hall in Terminal B", "checkpoint for gates B40–B49"}, []string{"AirTrain"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.fromTerminal, ArrivalGate: tc.fromGate}
			out := feeds.Flight{DepartureTerminal: tc.toTerminal, DepartureGate: tc.toGate}
			got := ewrConnectionDirections(in, out)
			if got.landside != tc.landside {
				t.Errorf("landside = %v, want %v", got.landside, tc.landside)
			}
			steps := strings.Join(got.steps, " ")
			for _, want := range tc.want {
				if !strings.Contains(steps, want) {
					t.Errorf("directions %q missing %q", steps, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(steps, absent) {
					t.Errorf("directions %q unexpectedly include %q", steps, absent)
				}
			}
		})
	}
}

func TestEWRConnectionDirectionsRequireUsableEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name, fromTerminal, fromGate, toTerminal, toGate string
	}{
		{"unknown terminal", "D", "", "C", "C72"},
		{"terminal conflicts with gate", "A", "B40", "C", "C72"},
		{"unknown Terminal A gate", "A", "A35", "C", "C72"},
		{"unknown Terminal C gate", "A", "A34", "C", "C124"},
		{"unknown Terminal B gate band", "B", "B50", "B", "B51"},
		{"missing Terminal B checkpoint", "B", "", "B", "B51"},
		{"unqualified numeric gate", "", "12", "C", "C72"},
		{"compound gate", "A", "A12/A14", "C", "C72"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ewrConnectionDirections(
				feeds.Flight{ArrivalTerminal: tc.fromTerminal, ArrivalGate: tc.fromGate},
				feeds.Flight{DepartureTerminal: tc.toTerminal, DepartureGate: tc.toGate},
			)
			if len(got.steps) != 0 || got.landside {
				t.Fatalf("ambiguous endpoint received directions: %+v", got)
			}
		})
	}
}

func TestEWRConnectionDirectionsUnitedShuttleIsConditional(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, inbound, outbound, shuttle string
	}{
		{"A to C", "A", "C", "UAL123", "ual456", "from gate A28 to gate C71"},
		{"C to A", "C", "A", "UAL123", "UAL456", "from gate C71 to gate A28"},
		{"unknown operating carrier", "A", "C", "UAL123", "", ""},
		{"malformed ICAO flight", "A", "C", "UAL", "UAL456", ""},
		{"different carrier", "A", "C", "UAL123", "AAL456", ""},
		{"Terminal B public route", "B", "C", "UAL123", "UAL456", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ewrConnectionDirections(
				feeds.Flight{ArrivalTerminal: tc.from, ICAONumber: tc.inbound},
				feeds.Flight{DepartureTerminal: tc.to, ICAONumber: tc.outbound},
			)
			steps := strings.Join(got.steps, " ")
			if !got.landside || !strings.Contains(steps, "free AirTrain") || !strings.Contains(steps, "Clear TSA") {
				t.Fatalf("expected a public route: %+v", got)
			}
			if tc.shuttle != "" {
				if !strings.Contains(steps, "If United confirms you are eligible and can stay past security") || !strings.Contains(steps, tc.shuttle) {
					t.Errorf("expected a conditional shuttle %q in %q", tc.shuttle, steps)
				}
			} else if strings.Contains(steps, "secure shuttle") {
				t.Errorf("unexpected secure shuttle in %q", steps)
			}
		})
	}
}
