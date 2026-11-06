package api

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

func denGateConcourse(terminal, gate string) byte {
	var reported byte
	terminal = connectionCode(terminal)
	switch terminal {
	case "", "JEPPESEN", "JEPPESEN TERMINAL", "EAST", "WEST", "TERMINAL EAST", "TERMINAL WEST":
		// Jeppesen and its east/west sides do not identify a concourse.
	default:
		terminal = strings.TrimPrefix(strings.TrimPrefix(terminal, "CONCOURSE "), "GATES ")
		terminal = strings.TrimSuffix(terminal, " GATES")
		if len(terminal) != 1 || terminal[0] < 'A' || terminal[0] > 'C' {
			return 0
		}
		reported = terminal[0]
	}
	gate = connectionCode(gate)
	if gate == "" {
		return reported
	}
	match := guideGatePattern.FindStringSubmatch(strings.TrimPrefix(gate, "GATE "))
	if match == nil || match[1][0] > 'C' {
		return 0
	}
	concourse := match[1][0]
	number, _ := strconv.Atoi(match[2])
	// These published ranges identify concourses. Exact gates and corridors
	// still come from the boarding pass and airport signs.
	limits := [...]struct{ first, last int }{{14, 87}, {7, 95}, {23, 71}}
	limit := limits[concourse-'A']
	if number < limit.first || number > limit.last || reported != 0 && reported != concourse {
		return 0
	}
	return concourse
}

func denConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	from := denGateConcourse(in.ArrivalTerminal, in.ArrivalGate)
	to := denGateConcourse(out.DepartureTerminal, out.DepartureGate)
	if from == 0 || to == 0 {
		return connectionGuideDirections{}
	}
	var steps []string
	if from == to {
		steps = append(steps, fmt.Sprintf("Follow departure gate signs within the %c gates.", to))
	} else {
		steps = append(steps, fmt.Sprintf("Follow signs to the Train to the Gates station in the center of the %c gates. Take the train to the %c gates.", from, to))
	}
	if from == 'A' {
		steps = append(steps, fmt.Sprintf("If you still need U.S. entry processing, follow signs to Customs and Border Protection in Jeppesen Terminal. Recheck bags as directed, clear TSA screening, then take the train to the %c gates.", to))
	}
	return connectionGuideDirections{steps: steps, secureSameTerminal: from == to}
}
