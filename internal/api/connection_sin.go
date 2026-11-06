package api

import (
	"fmt"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

func sinConnectionTerminal(terminal, gate string) string {
	terminal = connectionCode(terminal)
	if terminal != "" {
		if suffix, ok := strings.CutPrefix(terminal, "TERMINAL"); ok {
			terminal = strings.TrimSpace(suffix)
		} else {
			terminal = strings.TrimSpace(strings.TrimPrefix(terminal, "T"))
		}
		switch terminal {
		case "1", "2", "3", "4":
		default:
			return ""
		}
	}
	gate = strings.TrimPrefix(connectionCode(gate), "GATE ")
	if gate == "" {
		return terminal
	}
	// Changi's terminal maps group boarding gates by these prefixes.
	var fromGate string
	switch gateConcourse(gate, "ABCDEFG") {
	case "A", "B":
		fromGate = "3"
	case "C", "D":
		fromGate = "1"
	case "E", "F":
		fromGate = "2"
	case "G":
		fromGate = "4"
	}
	if terminal != "" && terminal != fromGate {
		return ""
	}
	return fromGate
}

func sinConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	from := sinConnectionTerminal(in.ArrivalTerminal, in.ArrivalGate)
	to := sinConnectionTerminal(out.DepartureTerminal, out.DepartureGate)
	if from == "" || to == "" {
		return connectionGuideDirections{}
	}
	steps := []string{"If both flights are on one itinerary and any checked bags are tagged to your final destination, follow the transit signs. Ask at a transfer desk if you need an onward boarding pass."}
	switch {
	case from == to:
		steps = append(steps, fmt.Sprintf("Check the airport screens and follow departure gate signs within Terminal %s.", to))
	case from == "4" || to == "4":
		stops := [...]string{"Gate C21", "Arrival Immigration Hall F", "Arrival Immigration Hall A", "the Transfer Lounge"}
		steps = append(steps, fmt.Sprintf("For an airside connection, take the free transit shuttle from %s in Terminal %s to Terminal %s. Allow time for the shuttle, walking, and screening.", stops[from[0]-'1'], from, to))
	default:
		steps = append(steps, fmt.Sprintf("Follow transit signs to Terminal %s. Check which Skytrain connection or airside walkway serves your onward gate.", to))
	}
	if to == "4" {
		steps = append(steps, "Follow signs for central security screening in Terminal 4 before heading to your gate.")
	} else {
		steps = append(steps, "Allow time for security screening at your departure gate.")
	}
	steps = append(steps, "On separate tickets, confirm the transfer with your airline. If you need to check in again or collect bags, follow Arrivals and meet Singapore's entry requirements before checking in for your onward flight.")
	return connectionGuideDirections{steps: steps, requiresScreening: true}
}
