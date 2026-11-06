package api

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

type ordGateArea struct {
	terminal, concourse string
}

func ordAreaForGate(terminal, gate string) ordGateArea {
	terminal = connectionCode(terminal)
	if terminal != "" {
		terminal = strings.TrimPrefix(strings.TrimPrefix(terminal, "TERMINAL "), "T")
		switch terminal {
		case "1", "2", "3", "5":
		default:
			return ordGateArea{}
		}
	}
	if connectionCode(gate) == "" {
		return ordGateArea{terminal: terminal}
	}

	// Chicago's facility directory identifies terminals by concourse letter.
	match := guideGatePattern.FindStringSubmatch(strings.TrimPrefix(connectionCode(gate), "GATE "))
	if match == nil {
		return ordGateArea{}
	}
	concourse := match[1]
	var inferred string
	switch concourse {
	case "B", "C":
		inferred = "1"
	case "E", "F":
		inferred = "2"
	case "G", "H", "K", "L":
		inferred = "3"
	case "M":
		// The published Terminal 5 map ends at M40.
		number, _ := strconv.Atoi(match[2])
		if number > 40 {
			return ordGateArea{}
		}
		inferred = "5"
	}
	if inferred == "" || terminal != "" && terminal != inferred {
		return ordGateArea{}
	}
	return ordGateArea{terminal: inferred, concourse: concourse}
}

func ordConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	from := ordAreaForGate(in.ArrivalTerminal, in.ArrivalGate)
	to := ordAreaForGate(out.DepartureTerminal, out.DepartureGate)
	if from.terminal == "" || to.terminal == "" {
		return connectionGuideDirections{}
	}
	if from.terminal == to.terminal {
		if to.terminal == "1" && from.concourse != "" && to.concourse != "" && from.concourse != to.concourse {
			return connectionGuideDirections{secureSameTerminal: true, steps: []string{
				fmt.Sprintf("Follow signs through the underground walkway from Concourse %s to Concourse %s in Terminal 1.", from.concourse, to.concourse),
			}}
		}
		return connectionGuideDirections{secureSameTerminal: true, steps: []string{
			fmt.Sprintf("Follow departure gate signs within Terminal %s.", to.terminal),
		}}
	}
	if from.terminal != "5" && to.terminal != "5" {
		return connectionGuideDirections{steps: []string{
			fmt.Sprintf("Follow the signed secure walkways from Terminal %s to Terminal %s, then follow departure gate signs.", from.terminal, to.terminal),
		}}
	}

	steps := []string{
		fmt.Sprintf("Follow signs from Terminal %s to the public Airport Transit System (ATS) station.", from.terminal),
		fmt.Sprintf("Take the free ATS to Terminal %s.", to.terminal),
		fmt.Sprintf("Clear TSA screening in Terminal %s, then follow signs to your departure gate.", to.terminal),
	}
	// The secure bus has direct routes only between Terminal 5 and 1 or 3.
	if from.terminal != "2" && to.terminal != "2" {
		steps = append([]string{fmt.Sprintf("If you can stay past security and have a same-day boarding pass for Terminal %s, confirm whether the secure Terminal Transfer Bus is running. Otherwise, use the public route below.", to.terminal)}, steps...)
	}
	return connectionGuideDirections{landside: true, steps: steps}
}
