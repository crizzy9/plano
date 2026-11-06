package api

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

func amsGateArea(value string) string {
	gate := strings.TrimSpace(strings.TrimPrefix(connectionCode(value), "GATE "))
	if len(gate) > 2 && gate[1] == ' ' {
		gate = gate[:1] + strings.TrimSpace(gate[1:])
	}
	match := guideGatePattern.FindStringSubmatch(gate)
	if match == nil {
		return ""
	}
	// Schiphol's lounge and transfer desk guides split Pier D between areas.
	switch match[1] {
	case "B", "C":
		return "Schengen"
	case "E", "F", "G", "H":
		return "non-Schengen"
	case "D":
		number, _ := strconv.Atoi(match[2]) // The gate pattern has already matched digits.
		switch {
		case number <= 57:
			return "non-Schengen"
		case number >= 59 && number <= 87:
			return "Schengen"
		}
	}
	return ""
}

func amsConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	from := amsGateArea(in.ArrivalGate)
	to := amsGateArea(out.DepartureGate)
	if from == "" || to == "" {
		return connectionGuideDirections{}
	}
	border := from != to
	first := fmt.Sprintf("Follow the yellow transfer signs within the %s area.", to)
	if border {
		first = fmt.Sprintf("If you can transfer airside, follow the yellow transfer signs through passport control to the %s area.", to)
	}
	return connectionGuideDirections{
		requiresBorderControl: border,
		steps: []string{
			first,
			"Follow signs and staff directions for transfer security if screening is required.",
			"Check the airport screens for your current departure gate before following gate signs.",
			"If you need to collect and recheck checked bags, follow Arrivals and allow time for check-in and departures security.",
		},
	}
}
