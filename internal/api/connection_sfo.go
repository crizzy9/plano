package api

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

// The airport's static maps identify the gate areas. Its Terminal 3
// construction guide retains the secure path between E, F, and G.
var sfoAreas = [...]struct {
	terminal    string
	first, last int
}{
	{"I", 1, 15}, // A
	{"1", 1, 27}, // B
	{"1", 1, 11}, // C
	{"2", 1, 18}, // D
	{"3", 2, 13}, // E
	{"3", 5, 22}, // F
	{"I", 1, 14}, // G
}

func sfoGateArea(terminal, gate string) byte {
	match := guideGatePattern.FindStringSubmatch(strings.TrimPrefix(connectionCode(gate), "GATE "))
	if match == nil || match[1][0] > 'G' {
		return 0
	}
	area := match[1][0]
	location := sfoAreas[area-'A']
	number, _ := strconv.Atoi(match[2])
	if number < location.first || number > location.last || area == 'D' && number == 13 {
		return 0
	}
	terminal = strings.TrimPrefix(connectionCode(terminal), "TERMINAL ")
	switch terminal {
	case "":
		return area
	case "T1", "T2", "T3":
		terminal = terminal[1:]
	case "HARVEY MILK TERMINAL 1":
		terminal = "1"
	case "INTERNATIONAL", "INTERNATIONAL TERMINAL", "INTL", "INTL TERMINAL":
		terminal = "I"
	case "A", "G", "INTERNATIONAL A", "INTERNATIONAL G":
		if terminal[len(terminal)-1] == area {
			return area
		}
		return 0
	}
	if terminal == location.terminal {
		return area
	}
	return 0
}

func sfoConnectionDirections(in, out feeds.Flight) []string {
	from, to := sfoGateArea(in.ArrivalTerminal, in.ArrivalGate), sfoGateArea(out.DepartureTerminal, out.DepartureGate)
	if from == 0 || to == 0 {
		return nil
	}
	if from == to {
		return []string{fmt.Sprintf("Follow departure gate signs within the %c gates.", to)}
	}
	// International A and G share a terminal name but sit at opposite ends
	// of the secure walkway, with the domestic gate areas in between.
	var waypoints []string
	direction := 1
	if from > to {
		direction = -1
	}
	for next := int(from) + direction; next != int(to); next += direction {
		waypoints = append(waypoints, string(rune(next)))
	}
	if len(waypoints) == 0 {
		return []string{fmt.Sprintf("Follow the secure walkway from the %c gates to the %c gates. Follow posted detours.", from, to)}
	}
	return []string{fmt.Sprintf("Follow secure walkway signs from the %c gates via gate areas %s to the %c gates. Follow posted detours.", from, strings.Join(waypoints, ", "), to)}
}
