package api

import (
	"fmt"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

func dxbConnectionLocation(terminal, gate string) (string, string) {
	terminal = connectionCode(terminal)
	if terminal != "" {
		if suffix, ok := strings.CutPrefix(terminal, "TERMINAL"); ok {
			terminal = strings.TrimSpace(suffix)
		} else {
			terminal = strings.TrimSpace(strings.TrimPrefix(terminal, "T"))
		}
		if terminal != "1" && terminal != "2" && terminal != "3" {
			return "", ""
		}
	}
	gate = strings.TrimSpace(strings.TrimPrefix(connectionCode(gate), "GATE "))
	if gate == "" {
		return terminal, ""
	}
	if letter, number, ok := strings.Cut(gate, " "); ok && len(letter) == 1 {
		gate = letter + strings.TrimSpace(number)
	}
	concourse := gateConcourse(gate, "ABCDF")
	var gateTerminal string
	switch concourse {
	case "D":
		gateTerminal = "1"
	case "F":
		gateTerminal = "2"
	case "A", "B", "C":
		gateTerminal = "3"
	}
	if terminal != "" && terminal != gateTerminal {
		return "", ""
	}
	return gateTerminal, concourse
}

func dxbConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	from, fromConcourse := dxbConnectionLocation(in.ArrivalTerminal, in.ArrivalGate)
	to, toConcourse := dxbConnectionLocation(out.DepartureTerminal, out.DepartureGate)
	if from == "" || to == "" {
		return connectionGuideDirections{}
	}

	steps := []string{
		"Follow Connections signs if your airline confirms you can remain in transit. Ask at the transfer desk if you need your onward boarding pass.",
		"Follow posted signs for any required transfer security along your route.",
	}
	var transferMinutes int
	terminalTwoThree := from == "2" && to == "3" || from == "3" && to == "2"
	switch {
	case terminalTwoThree:
		// flydubai's Terminal 2–3 guidance allows 120 minutes for the connection.
		transferMinutes = 120 - defaultConnectionBoardingReserveMinutes
		steps = append(steps, fmt.Sprintf("Ask the transfer desk if you can take the airside connections bus to Terminal %s and where to board it.", to))
	case from == "3" && to == "1":
		steps = append(steps, "Ask the transfer desk if your airline can direct you to the airside shuttle for Terminal 1 D gates.")
	case from != to:
		steps = append(steps, fmt.Sprintf("Ask your airline or airport staff which transfer route is available to Terminal %s before leaving the secure area.", to))
	case from != "3" || fromConcourse == "" || toConcourse == "" || fromConcourse == toConcourse:
		steps = append(steps, fmt.Sprintf("Check the airport screens for your onward gate and follow departure signs within Terminal %s.", to))
	case fromConcourse == "A" && toConcourse == "C" || fromConcourse == "C" && toConcourse == "A":
		steps = append(steps, "Follow signs for the airport train serving A gates and the walkway between B and C gates. Ask the transfer desk if the Emirates shuttle between A and C serves your connection.")
	case fromConcourse == "A" || toConcourse == "A":
		steps = append(steps, fmt.Sprintf("Follow signs for the Terminal 3 airport train and the %s gates.", toConcourse))
	default:
		steps = append(steps, fmt.Sprintf("Follow the Terminal 3 walkway signs to the %s gates.", toConcourse))
	}
	steps = append(steps, "If you need to collect bags or check in again, ask the transfer desk about baggage assistance and check your UAE entry requirements. If you need to enter the UAE, follow Arrivals through passport control, collect any checked bags, clear customs, and go to public check-in and departure security in your onward terminal.")
	return connectionGuideDirections{steps: steps, transferMinutes: transferMinutes, requiresScreening: true}
}
