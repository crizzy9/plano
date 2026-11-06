package api

import (
	"fmt"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

// Terminal 3's first day of passenger operations was April 23, 2026 in Frankfurt.
var fraTerminal3Opened = time.Date(2026, time.April, 22, 22, 0, 0, 0, time.UTC)

func fraConnectionLocation(terminal, gate string, at *time.Time) (string, string) {
	terminal = connectionCode(terminal)
	if suffix, ok := strings.CutPrefix(terminal, "TERMINAL"); ok {
		terminal = strings.TrimSpace(suffix)
	} else {
		terminal = strings.TrimSpace(strings.TrimPrefix(terminal, "T"))
	}
	if terminal != "" && terminal != "1" && terminal != "3" {
		return "", ""
	}

	var pier string
	gate = strings.TrimSpace(strings.TrimPrefix(connectionCode(gate), "GATE "))
	if gate != "" {
		if letter, number, ok := strings.Cut(gate, " "); ok && len(letter) == 1 {
			gate = letter + strings.TrimSpace(number)
		}
		pier = gateConcourse(gate, "ABCZGHJ")
		if pier == "" {
			return "", ""
		}
		gateTerminal := "1"
		if strings.Contains("GHJ", pier) {
			gateTerminal = "3"
		}
		if terminal != "" && terminal != gateTerminal {
			return "", ""
		}
		terminal = gateTerminal
	}
	if terminal == "3" && at != nil && at.Before(fraTerminal3Opened) {
		return "", ""
	}
	return terminal, pier
}

func fraPassportArea(pier string) string {
	switch pier {
	case "A", "H":
		return "Schengen"
	case "Z", "J":
		return "non-Schengen"
	default:
		// Pier B and Pier G serve both areas. A pier prefix alone cannot
		// identify the transfer controls there.
		return ""
	}
}

func fraConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	arrival := first(in.ActualArrival, in.ActualLanding, in.EstimatedArrival, in.ScheduledArrival)
	from, fromPier := fraConnectionLocation(in.ArrivalTerminal, in.ArrivalGate, arrival)
	to, toPier := fraConnectionLocation(out.DepartureTerminal, out.DepartureGate, connectionDeparture(out))
	if from == "" || to == "" {
		return connectionGuideDirections{}
	}

	steps := []string{"Check the airport screens for your onward gate and the time to reach it."}
	changesTerminal := from != to
	var transferMinutes int
	switch {
	case changesTerminal:
		transferMinutes = defaultConnectionTerminalTransferMinutes
		steps = append(steps,
			fmt.Sprintf("Follow SkyLine signs from Terminal %s to Terminal %s and check which platform serves your connection.", from, to),
			"When the SkyLine is out of service, ask transfer staff which route you can use before leaving the secure area. The replacement shuttle stops outside Arrivals.")
	case fromPier == "A" && toPier == "B" || fromPier == "B" && toPier == "A":
		steps = append(steps, "If airport signs route you between Piers A and B in the Schengen area, use the passenger tunnel between A15 and B19.")
	default:
		steps = append(steps, fmt.Sprintf("Follow transfer signs within Terminal %s.", to))
	}

	fromArea, toArea := fraPassportArea(fromPier), fraPassportArea(toPier)
	borderControl := fromArea != "" && toArea != "" && fromArea != toArea
	if borderControl {
		steps = append(steps, "Clear passport control when crossing between the Schengen and non-Schengen areas.")
	}
	steps = append(steps,
		"If you need an onward boarding pass, ask your airline at its transfer desk.",
		"Follow airport signs for any required transfer security.",
		"If you need to collect bags or check in again, follow Arrivals and allow time for any required immigration, baggage recheck, and departures security.")
	return connectionGuideDirections{
		steps: steps, transferMinutes: transferMinutes, requiresBorderControl: borderControl,
		secureSameTerminal: !changesTerminal && fromPier == toPier && fromArea != "",
	}
}
