package api

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

func dohConnectionConcourse(terminal, gate string) (string, bool) {
	terminal = connectionCode(terminal)
	switch terminal {
	case "", "1", "T1", "T 1", "TERMINAL1", "TERMINAL 1", "MAIN", "MAIN TERMINAL":
	default:
		return "", false
	}
	gate = connectionCode(gate)
	if gate == "" {
		return "", terminal != ""
	}
	gate = strings.TrimSpace(strings.TrimPrefix(gate, "GATE"))
	if letter, number, ok := strings.Cut(gate, " "); ok && len(letter) == 1 {
		gate = letter + strings.TrimSpace(number)
	}
	match := guideGatePattern.FindStringSubmatch(gate)
	if match == nil {
		return "", false
	}
	number, _ := strconv.Atoi(match[2])
	// JAL's terminal map identifies the main gate ranges. Hamad's detailed
	// maps list the Concourse C groups and level 0 gates 20–24 in D and E.
	concourse := match[1]
	switch {
	case concourse == "A" && number <= 11,
		concourse == "B" && number <= 10,
		concourse == "C" && (number <= 43 || number >= 50 && number <= 65 || number >= 80 && number <= 93),
		concourse == "D" && (number <= 9 || number >= 20 && number <= 24),
		concourse == "E" && (number <= 8 || number >= 20 && number <= 24):
		return concourse, true
	}
	return "", false
}

func dohConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	_, fromKnown := dohConnectionConcourse(in.ArrivalTerminal, in.ArrivalGate)
	to, toKnown := dohConnectionConcourse(out.DepartureTerminal, out.DepartureGate)
	if !fromKnown || !toKnown {
		return connectionGuideDirections{}
	}
	steps := []string{
		"If your airline confirms you can stay in transit, follow Transfers signs from your arrival gate and clear transfer security. Ask at your airline's transfer desk if you need an onward boarding pass.",
		"Check the departure screens for your current gate.",
	}
	if to == "C" || to == "D" || to == "E" {
		steps = append(steps, "Check the Passenger Train signs to see whether the shuttle serves your current gate.")
	}
	if to == "" {
		steps = append(steps, "Follow signs to your confirmed departure gate.")
	} else {
		steps = append(steps, fmt.Sprintf("If your departure is still in Concourse %s, follow the signs to your gate.", to))
	}
	steps = append(steps,
		"If you need to collect bags or check in at public departures, confirm your eligibility to enter Qatar with your airline. Allow time for immigration, baggage claim, customs, check-in, and departure screening.")
	return connectionGuideDirections{steps: steps, requiresScreening: true}
}
