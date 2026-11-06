package api

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

type laxGateArea int

// These areas follow the secure walkway order. The T4–T6 link uses a bus,
// and the West Gates branch off Terminal B through the passenger tunnel.
const (
	laxUnknown laxGateArea = iota
	laxTerminal1
	laxTerminal2
	laxTerminal3
	laxTerminalB
	laxTerminal4
	laxTerminal6
	laxTerminal7
	laxTerminal8
	laxWestGates
)

// Use the airport's December 2025 connector diagram, with conservative gate
// ranges for the T2/T3 complex. The 50s include remote gates and closed T5.
// https://www.flylax.com/sites/lax/files/2025-12/Terminal%20Connections%20Pedestrian%20Path%2025-1222.pdf
var laxGateAreas = [...]struct {
	label, terminal string
	first, last     int
}{
	laxTerminal1: {"Terminal 1", "1", 9, 18},
	laxTerminal2: {"the Terminal 2 gate area", "2", 20, 28},
	laxTerminal3: {"Terminal 3", "3", 30, 38},
	laxTerminalB: {"Terminal B", "B", 130, 159},
	laxTerminal4: {"Terminal 4", "4", 40, 49},
	laxTerminal6: {"Terminal 6", "6", 60, 69},
	laxTerminal7: {"Terminal 7", "7", 70, 77},
	laxTerminal8: {"Terminal 8", "8", 80, 88},
	laxWestGates: {"the West Gates", "B", 201, 235},
}

var laxGatePattern = regexp.MustCompile(`^([1-9][0-9]{0,2})[A-Z]?$`)

func laxAreaForGate(terminal, gate string) laxGateArea {
	match := laxGatePattern.FindStringSubmatch(strings.TrimPrefix(connectionCode(gate), "GATE "))
	if match == nil {
		return laxUnknown
	}
	number, _ := strconv.Atoi(match[1])
	terminal = strings.TrimPrefix(connectionCode(terminal), "TERMINAL ")
	if len(terminal) == 2 && terminal[0] == 'T' && terminal[1] >= '1' && terminal[1] <= '8' {
		terminal = terminal[1:]
	}
	switch terminal {
	case "TBIT", "INTL", "INTERNATIONAL", "INTERNATIONAL TERMINAL", "TOM BRADLEY INTERNATIONAL", "TOM BRADLEY INTERNATIONAL TERMINAL":
		terminal = "B"
	}
	for i, candidate := range laxGateAreas {
		if number < candidate.first || number > candidate.last {
			continue
		}
		area := laxGateArea(i)
		switch {
		case terminal == "" || terminal == candidate.terminal:
			return area
		case area == laxTerminal2 && terminal == "3":
			// FlyLAX identifies the 20–28 gates as part of the Terminal 3 complex.
			return area
		case area == laxWestGates && (terminal == "WEST" || terminal == "WEST GATES" || terminal == "BRADLEY WEST" || terminal == "BRADLEY WEST GATES"):
			return area
		default:
			return laxUnknown
		}
	}
	return laxUnknown
}

func laxConnectionDirections(in, out feeds.Flight) []string {
	from, to := laxAreaForGate(in.ArrivalTerminal, in.ArrivalGate), laxAreaForGate(out.DepartureTerminal, out.DepartureGate)
	if from == laxUnknown || to == laxUnknown {
		return nil
	}
	if from == to {
		return []string{fmt.Sprintf("Follow departure gate signs within %s.", laxGateAreas[to].label)}
	}
	var steps []string
	if from == laxWestGates {
		steps = append(steps, "Follow signs through the Gateway passenger tunnel from the West Gates to Terminal B.")
		from = laxTerminalB
	}
	toWestGates := to == laxWestGates
	if toWestGates {
		to = laxTerminalB
	}
	if (from <= laxTerminal4) != (to <= laxTerminal4) {
		// LAWA designated an airfield bus when the T5 tunnel closed.
		// https://www.lawa.org/sites/lawa/files/2025-10/Construction%20Hot%20Topics_10.27.25.pdf
		busFrom, busTo := laxTerminal4, laxTerminal6
		if from >= laxTerminal6 {
			busFrom, busTo = busTo, busFrom
		}
		steps = appendLaxWalk(steps, from, busFrom)
		steps = append(steps, fmt.Sprintf("Follow signs for the secure airfield bus from %s to %s. Confirm the current boarding location with airport staff.",
			laxGateAreas[busFrom].label, laxGateAreas[busTo].label))
		from = busTo
	}
	steps = appendLaxWalk(steps, from, to)
	if toWestGates {
		steps = append(steps, "Follow signs through the Gateway passenger tunnel from Terminal B to the West Gates.")
	}
	return steps
}

func appendLaxWalk(steps []string, from, to laxGateArea) []string {
	if from == to {
		return steps
	}
	direction := laxGateArea(1)
	if from > to {
		direction = -1
	}
	var via []string
	for area := from + direction; area != to; area += direction {
		via = append(via, laxGateAreas[area].label)
	}
	walk := fmt.Sprintf("Follow signed secure walkways from %s to %s", laxGateAreas[from].label, laxGateAreas[to].label)
	if len(via) != 0 {
		walk += " via " + strings.Join(via, ", ")
	}
	return append(steps, walk+".")
}
