package api

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

type jfkGateArea struct {
	terminal, concourse string
}

var jfkGatePattern = regexp.MustCompile(`^([AB]?)([1-9][0-9]{0,2})[A-Z]?$`)

func jfkAreaForGate(terminal, gate string) jfkGateArea {
	terminal = connectionCode(terminal)
	if terminal != "" {
		terminal = strings.TrimPrefix(strings.TrimPrefix(terminal, "TERMINAL "), "T")
		switch terminal {
		case "1", "4", "5", "7", "8":
		default:
			return jfkGateArea{}
		}
	}

	gate = strings.TrimPrefix(connectionCode(gate), "GATE ")
	if gate == "" {
		return jfkGateArea{terminal: terminal}
	}
	match := jfkGatePattern.FindStringSubmatch(gate)
	if match == nil {
		return jfkGateArea{}
	}
	letter := match[1]
	number, _ := strconv.Atoi(match[2])
	// JFK's published gate ranges distinguish T4 and T5. The low numeric
	// gates overlap across T1, T7, and T8, so require the reported terminal.
	switch {
	case letter == "A" && number >= 3 && number <= 21,
		letter == "B" && number >= 18 && number <= 55:
		if terminal == "" || terminal == "4" {
			return jfkGateArea{terminal: "4", concourse: letter}
		}
	case letter == "" && number >= 501 && number <= 530:
		if terminal == "" || terminal == "5" {
			return jfkGateArea{terminal: "5"}
		}
	case letter == "" && terminal == "1" && number <= 11,
		letter == "" && terminal == "7" && number <= 12:
		return jfkGateArea{terminal: terminal}
	case letter == "" && terminal == "8":
		if number <= 10 || number >= 12 && number <= 20 {
			return jfkGateArea{terminal: "8", concourse: "B"}
		}
		if number >= 31 && number <= 47 {
			return jfkGateArea{terminal: "8", concourse: "C"}
		}
	}
	return jfkGateArea{}
}

func jfkConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	from := jfkAreaForGate(in.ArrivalTerminal, in.ArrivalGate)
	to := jfkAreaForGate(out.DepartureTerminal, out.DepartureGate)
	if from.terminal == "" || to.terminal == "" {
		return connectionGuideDirections{}
	}
	if from.terminal != to.terminal {
		return connectionGuideDirections{landside: true, steps: []string{
			fmt.Sprintf("Follow signs from Terminal %s to the public AirTrain station.", from.terminal),
			fmt.Sprintf("Take the free AirTrain to Terminal %s. Check station signs for the correct train or replacement service.", to.terminal),
			fmt.Sprintf("Clear TSA screening in Terminal %s, then follow signs to your departure gate.", to.terminal),
		}}
	}
	if from.concourse != "" && to.concourse != "" && from.concourse != to.concourse {
		if to.terminal == "4" {
			return connectionGuideDirections{secureSameTerminal: true, steps: []string{fmt.Sprintf("Follow signs through the main concourse junction from Concourse %s to Concourse %s in Terminal 4.", from.concourse, to.concourse)}}
		}
		return connectionGuideDirections{secureSameTerminal: true, steps: []string{fmt.Sprintf("Follow signs through the underground walkway from Concourse %s to Concourse %s in Terminal 8.", from.concourse, to.concourse)}}
	}
	return connectionGuideDirections{secureSameTerminal: true, steps: []string{fmt.Sprintf("Follow departure gate signs within Terminal %s.", to.terminal)}}
}
