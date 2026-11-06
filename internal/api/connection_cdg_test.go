package api

import (
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestCDGConnectionDirections(t *testing.T) {
	beforeRename := time.Date(2027, time.March, 15, 22, 59, 59, 0, time.UTC)
	atRename := beforeRename.Add(time.Second)
	for _, tc := range []struct {
		name, fromTerminal, fromGate, toTerminal, toGate string
		depart                                           *time.Time
		wantDestination                                  string
		wantBorder, wantOrange, wantLegacy               bool
	}{
		{name: "leave Schengen", fromTerminal: "2F", fromGate: "F26", toTerminal: "Terminal 2E", toGate: "Gate K35",
			depart: &beforeRename, wantDestination: "Terminal 2E, Gates K", wantBorder: true},
		{name: "enter Schengen", fromGate: "L32", toTerminal: "T2G", toGate: "G31",
			depart: &beforeRename, wantDestination: "Terminal 2G, Gates G", wantBorder: true},
		{name: "between 2E halls", fromTerminal: "2E", fromGate: "K35", toTerminal: "2E", toGate: "M45",
			depart: &beforeRename, wantDestination: "Terminal 2E, Gates M"},
		{name: "between Schengen terminals", fromTerminal: "2G", toTerminal: "2F",
			depart: &beforeRename, wantDestination: "Terminal 2F", wantOrange: true},
		{name: "Paris midnight uses new signs and accepts old flight labels", fromTerminal: "2F", fromGate: "F26", toTerminal: "2E", toGate: "K35",
			depart: &atRename, wantDestination: "Terminal 5, Gates D", wantBorder: true, wantLegacy: true},
		{name: "mixed labels after rename", fromTerminal: "6", fromGate: "F26", toTerminal: "2E", toGate: "C32",
			depart: &atRename, wantDestination: "Terminal 5, Gates C", wantBorder: true, wantLegacy: true},
		{name: "legacy hall without terminal still unique", fromGate: "K35", toTerminal: "5", toGate: "B45",
			depart: &atRename, wantDestination: "Terminal 5, Gates B", wantLegacy: true},
		{name: "explicit new terminal without date", fromTerminal: "Terminal 7", fromGate: "N31", toTerminal: "T6", toGate: "E26",
			wantDestination: "Terminal 6, Gates E", wantOrange: true, wantLegacy: true},
		{name: "numbered terminal 7 and its gate", fromTerminal: "6", fromGate: "E26", toTerminal: "7", toGate: "N31",
			depart: &atRename, wantDestination: "Terminal 7, Gates N", wantOrange: true, wantLegacy: true},
		{name: "conflicting old gate", fromTerminal: "2F", fromGate: "K35", toTerminal: "2G"},
		{name: "conflicting new gate", fromTerminal: "7", fromGate: "E26", toTerminal: "5", toGate: "D35", depart: &atRename},
		{name: "new gate with no terminal or date", fromTerminal: "2F", toGate: "B45"},
		{name: "new gate with date only can be a stale label", fromTerminal: "2F", toGate: "B45", depart: &atRename},
		{name: "old gate letter reused outside these zones", fromGate: "F26", toTerminal: "7", depart: &atRename},
		{name: "explicit legacy terminal and new gate before rename", fromTerminal: "2F", toTerminal: "2E", toGate: "D35", depart: &beforeRename},
		{name: "unsupported terminal", fromTerminal: "2C", toTerminal: "2F"},
		{name: "unsupported new terminal with reused gate", fromTerminal: "3", fromGate: "F26", toTerminal: "7", depart: &atRename},
		{name: "malformed terminal", fromTerminal: "T", fromGate: "K35", toTerminal: "2F"},
		{name: "missing arrival endpoint", toTerminal: "2F"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := feeds.Flight{ArrivalTerminal: tc.fromTerminal, ArrivalGate: tc.fromGate}
			out := feeds.Flight{DepartureTerminal: tc.toTerminal, DepartureGate: tc.toGate, ScheduledDeparture: tc.depart}
			got := cdgConnectionDirections(in, out)
			if tc.wantDestination == "" {
				if len(got.steps) != 0 {
					t.Fatalf("ambiguous or unsupported route received directions: %+v", got)
				}
				return
			}
			steps := strings.Join(got.steps, " ")
			if !strings.Contains(steps, "yellow Connections signs to "+tc.wantDestination+".") ||
				!strings.Contains(steps, "Clear transfer security if directed.") || got.landside || got.requiresScreening {
				t.Fatalf("unexpected wayfinding: %+v", got)
			}
			if got.requiresBorderControl != tc.wantBorder || strings.Contains(steps, "passport control") != tc.wantBorder {
				t.Fatalf("unexpected border control: %+v", got)
			}
			if strings.Contains(steps, "secure Orange shuttle is running and available") != tc.wantOrange ||
				strings.Contains(steps, "outside the secure area, ask where to board the public N2 shuttle") != tc.wantOrange {
				t.Fatalf("unexpected shuttle directions: %+v", got)
			}
			if strings.Contains(steps, "Older travel documents") != tc.wantLegacy {
				t.Fatalf("unexpected legacy signage guidance: %+v", got)
			}
		})
	}
}
