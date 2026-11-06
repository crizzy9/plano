package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestORDConnectionDirectionsUsePublishedRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, fromTerminal, fromGate, toTerminal, toGate string
		landside, sameTerminal                           bool
		want, absent                                     []string
	}{
		{
			name: "secure walk between core terminals", fromGate: "Gate C18B", toGate: " k20 ",
			want: []string{"signed secure walkways from Terminal 1 to Terminal 3"}, absent: []string{"ATS", "TSA"},
		},
		{
			name: "secure walk between known terminals", fromTerminal: "Terminal 2", toTerminal: "T1",
			want: []string{"signed secure walkways from Terminal 2 to Terminal 1"},
		},
		{
			name: "Terminal 1 underground connector", fromTerminal: "1", fromGate: "B10", toGate: "C18",
			sameTerminal: true, want: []string{"underground walkway from Concourse B to Concourse C"},
		},
		{
			name: "same Terminal 5", fromGate: "M13", toTerminal: "Terminal 5", toGate: "M28",
			sameTerminal: true, want: []string{"departure gate signs within Terminal 5"},
		},
		{
			name: "public route to Terminal 5 with conditional bus", fromGate: "B10", toGate: "M13", landside: true,
			want: []string{"same-day boarding pass for Terminal 5", "Terminal Transfer Bus is running", "free ATS to Terminal 5", "TSA screening in Terminal 5"},
		},
		{
			name: "public route from Terminal 5 with conditional bus", fromGate: "M13", toGate: "H5", landside: true,
			want: []string{"same-day boarding pass for Terminal 3", "Terminal Transfer Bus is running", "free ATS to Terminal 3", "TSA screening in Terminal 3"},
		},
		{
			name: "Terminal 2 public route", fromGate: "M13", toGate: "F8", landside: true,
			want: []string{"free ATS to Terminal 2", "TSA screening in Terminal 2"}, absent: []string{"Terminal Transfer Bus"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.fromTerminal, ArrivalGate: tc.fromGate}
			out := feeds.Flight{DepartureTerminal: tc.toTerminal, DepartureGate: tc.toGate}
			got := ordConnectionDirections(in, out)
			if got.landside != tc.landside || got.secureSameTerminal != tc.sameTerminal {
				t.Fatalf("directions = %+v, want landside = %t and secure same terminal = %t", got, tc.landside, tc.sameTerminal)
			}
			directions := strings.Join(got.steps, " ")
			for _, want := range tc.want {
				if !strings.Contains(directions, want) {
					t.Errorf("directions = %q, missing %q", got.steps, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(directions, absent) {
					t.Errorf("directions = %q, unexpectedly contain %q", got.steps, absent)
				}
			}
		})
	}
}

func TestORDConnectionDirectionsRequireCompatibleEndpoints(t *testing.T) {
	for _, tc := range []struct{ name, terminal, gate string }{
		{"missing location", "", ""},
		{"unknown terminal", "4", ""},
		{"incomplete terminal label", "T", "B10"},
		{"unrecognized concourse", "", "A8"},
		{"numeric gate without concourse", "5", "13"},
		{"conflicting terminal and gate", "T2", "M13"},
		{"outside Terminal 5 gate range", "", "M99"},
		{"invalid gate format", "3", "G1-G2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.terminal, ArrivalGate: tc.gate}
			out := feeds.Flight{DepartureTerminal: "1", DepartureGate: "B10"}
			if got := ordConnectionDirections(in, out); len(got.steps) != 0 || got.landside || got.secureSameTerminal {
				t.Fatalf("unexpected arrival directions %+v", got)
			}
			in = feeds.Flight{ArrivalTerminal: "1", ArrivalGate: "B10"}
			out = feeds.Flight{DepartureTerminal: tc.terminal, DepartureGate: tc.gate}
			if got := ordConnectionDirections(in, out); len(got.steps) != 0 || got.landside || got.secureSameTerminal {
				t.Fatalf("unexpected departure directions %+v", got)
			}
		})
	}
}
