package api

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

var (
	ewrGatePattern   = regexp.MustCompile(`^([ABC]?)(0?[1-9]|[1-9][0-9]{1,2})[A-Z]?$`)
	ewrUnitedPattern = regexp.MustCompile(`^UAL[0-9]+$`)
)

type ewrEndpoint struct {
	terminal, checkpoint string
}

func ewrConnectionEndpoint(terminal, gate string) ewrEndpoint {
	terminal = strings.TrimPrefix(connectionCode(terminal), "TERMINAL ")
	switch terminal {
	case "", "A", "B", "C":
	default:
		return ewrEndpoint{}
	}
	gate = strings.TrimPrefix(connectionCode(gate), "GATE ")
	if gate == "" {
		return ewrEndpoint{terminal: terminal}
	}
	match := ewrGatePattern.FindStringSubmatch(gate)
	if match == nil || (terminal != "" && match[1] != "" && terminal != match[1]) {
		return ewrEndpoint{}
	}
	if terminal == "" {
		terminal = match[1]
	}
	number, _ := strconv.Atoi(match[2])
	// The airport's terminal guides distinguish Terminal B's three TSA checkpoints.
	switch {
	case terminal == "A" && number >= 1 && number <= 34,
		terminal == "C" && (number >= 70 && number <= 123 || number >= 125 && number <= 139):
		return ewrEndpoint{terminal: terminal}
	case terminal == "B" && number >= 40 && number <= 49:
		return ewrEndpoint{terminal: terminal, checkpoint: "B40–B49"}
	case terminal == "B" && number >= 51 && number <= 57:
		return ewrEndpoint{terminal: terminal, checkpoint: "B51–B57"}
	case terminal == "B" && number >= 60 && number <= 68:
		return ewrEndpoint{terminal: terminal, checkpoint: "B60–B68"}
	default:
		return ewrEndpoint{}
	}
}

func ewrConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	from := ewrConnectionEndpoint(in.ArrivalTerminal, in.ArrivalGate)
	to := ewrConnectionEndpoint(out.DepartureTerminal, out.DepartureGate)
	if from.terminal == "" || to.terminal == "" {
		return connectionGuideDirections{}
	}
	if from.terminal == to.terminal {
		if to.terminal != "B" {
			return connectionGuideDirections{secureSameTerminal: true, steps: []string{fmt.Sprintf("Follow departure gate signs within Terminal %s.", to.terminal)}}
		}
		if from.checkpoint == "" || to.checkpoint == "" {
			return connectionGuideDirections{}
		}
		if from.checkpoint == to.checkpoint {
			return connectionGuideDirections{secureSameTerminal: true, steps: []string{fmt.Sprintf("Follow departure gate signs within the %s gate area.", to.checkpoint)}}
		}
		return connectionGuideDirections{landside: true, steps: []string{
			"Exit the secure area and follow signs to the public departures hall in Terminal B.",
			fmt.Sprintf("Clear TSA at the checkpoint for gates %s, then follow departure gate signs.", to.checkpoint),
		}}
	}

	steps := make([]string, 0, 5)
	if (from.terminal == "A" && to.terminal == "C" || from.terminal == "C" && to.terminal == "A") &&
		ewrUnitedPattern.MatchString(connectionCode(in.ICAONumber)) && ewrUnitedPattern.MatchString(connectionCode(out.ICAONumber)) {
		fromGate, toGate := "A28", "C71"
		if from.terminal == "C" {
			fromGate, toGate = toGate, fromGate
		}
		steps = append(steps, fmt.Sprintf("If United confirms you are eligible and can stay past security, ask about its secure shuttle from gate %s to gate %s. Otherwise, use the public route below.", fromGate, toGate))
	}
	steps = append(steps, "Exit the secure area and follow signs for the public terminal transfer.")
	if from.terminal == "A" {
		steps = append(steps, "Check posted service at Terminal A before leaving the terminal. If AirTrain is serving Terminal A, follow signs for the covered walk or public shuttle from Terminal A to its AirTrain station.")
	}
	steps = append(steps, fmt.Sprintf("Take the free AirTrain toward Terminal %s or follow posted signs for replacement service.", to.terminal))
	if to.terminal == "A" {
		steps = append(steps, "If you get off at the Terminal A AirTrain station, use the covered walk or signed public shuttle to Terminal A departures.")
	}
	if to.checkpoint != "" {
		steps = append(steps, fmt.Sprintf("In Terminal B, clear TSA at the checkpoint for gates %s and follow departure gate signs.", to.checkpoint))
	} else {
		steps = append(steps, fmt.Sprintf("Clear TSA in Terminal %s, then follow signs for your departure gate.", to.terminal))
	}
	return connectionGuideDirections{steps: steps, landside: true}
}
