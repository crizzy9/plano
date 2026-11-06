package api

import (
	"fmt"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

// The names change at midnight in Paris on March 16, 2027 (CET).
var cdgTerminalRename = time.Date(2027, time.March, 15, 23, 0, 0, 0, time.UTC)

type cdgTerminalZone struct {
	oldTerminal, newTerminal string
	oldGates, newGates       string
	nonSchengen              bool
}

var cdgTerminalZones = []cdgTerminalZone{
	{oldTerminal: "2E", newTerminal: "5", oldGates: "KLM", newGates: "DCB", nonSchengen: true},
	{oldTerminal: "2F", newTerminal: "6", oldGates: "F", newGates: "E"},
	{oldTerminal: "2G", newTerminal: "7", oldGates: "G", newGates: "N"},
}

type cdgConnectionLocation struct {
	zone *cdgTerminalZone
	hall int
}

func cdgTerminalCode(value string) string {
	terminal := connectionCode(value)
	if suffix, ok := strings.CutPrefix(terminal, "TERMINAL "); ok {
		return strings.TrimSpace(suffix)
	}
	if len(terminal) > 1 {
		return strings.TrimSpace(strings.TrimPrefix(terminal, "T"))
	}
	return terminal
}

func cdgLocation(terminal, gate string, renamed bool) cdgConnectionLocation {
	gate = strings.TrimPrefix(connectionCode(gate), "GATE ")
	family := gate
	if len(gate) > 1 {
		family = gateConcourse(gate, "KLMFGDCBEN")
		if family == "" {
			return cdgConnectionLocation{}
		}
	}
	// Gates K, L, and M remain unambiguous as old labels. Gates F and G
	// are reused at other terminals, and B, C, D, E, and N need a terminal.
	if terminal == "" && (family == "" || !strings.Contains("KLM", family)) {
		return cdgConnectionLocation{}
	}
	for i := range cdgTerminalZones {
		zone := &cdgTerminalZones[i]
		if terminal != "" && terminal != zone.oldTerminal && terminal != zone.newTerminal {
			continue
		}
		if family == "" {
			return cdgConnectionLocation{zone: zone, hall: -1}
		}
		hall := strings.Index(zone.oldGates, family)
		// Renamed gate families can overlap other facilities' legacy names.
		if hall < 0 && terminal != "" && (renamed || terminal == zone.newTerminal) {
			hall = strings.Index(zone.newGates, family)
		}
		if hall >= 0 {
			return cdgConnectionLocation{zone: zone, hall: hall}
		}
	}
	return cdgConnectionLocation{}
}

func (location cdgConnectionLocation) label(renamed bool) string {
	terminal, gates := location.zone.oldTerminal, location.zone.oldGates
	if renamed {
		terminal, gates = location.zone.newTerminal, location.zone.newGates
	}
	label := "Terminal " + terminal
	if location.hall >= 0 {
		label += ", Gates " + string(gates[location.hall])
	}
	return label
}

func cdgConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	arrivalTerminal, departureTerminal := cdgTerminalCode(in.ArrivalTerminal), cdgTerminalCode(out.DepartureTerminal)
	renamed := false
	if departure := connectionDeparture(out); departure != nil {
		renamed = !departure.Before(cdgTerminalRename)
	} else {
		for _, zone := range cdgTerminalZones {
			renamed = renamed || arrivalTerminal == zone.newTerminal || departureTerminal == zone.newTerminal
		}
	}
	from := cdgLocation(arrivalTerminal, in.ArrivalGate, renamed)
	to := cdgLocation(departureTerminal, out.DepartureGate, renamed)
	if from.zone == nil || to.zone == nil {
		return connectionGuideDirections{}
	}
	steps := []string{
		"Check your onward boarding gate on the airport screens.",
		fmt.Sprintf("If your airline confirms you can transfer airside, follow the yellow Connections signs to %s.", to.label(renamed)),
	}
	if renamed {
		steps = append(steps, fmt.Sprintf("Older travel documents may show %s.", to.label(false)))
	}
	borderControl := from.zone.nonSchengen != to.zone.nonSchengen
	if borderControl {
		steps = append(steps, "Clear passport control when crossing between the Schengen and non-Schengen areas.")
	}
	schengenTerminalChange := !from.zone.nonSchengen && !to.zone.nonSchengen && from.zone != to.zone
	if schengenTerminalChange {
		steps = append(steps, "Ask airport staff whether the secure Orange shuttle is running and available for your connection.")
	}
	steps = append(steps,
		"Clear transfer security if directed.",
		"If you need to collect bags or check in again, follow Arrivals and ask your airline where to check in for your next flight.")
	if schengenTerminalChange {
		steps = append(steps, "If you have to transfer between these terminals outside the secure area, ask where to board the public N2 shuttle.")
	}
	// Groupe ADP's April 2026 business guide, page 34, gives 90 minutes for
	// supported pairs, except for changes involving 2G, which use 120 minutes.
	// The generic risk assessment adds its boarding reserve separately.
	planningMinutes := 90
	if from.zone != to.zone && (from.zone.oldTerminal == "2G" || to.zone.oldTerminal == "2G") {
		planningMinutes = 120
	}
	return connectionGuideDirections{steps: steps, requiresBorderControl: borderControl,
		transferMinutes: planningMinutes - defaultConnectionBoardingReserveMinutes}
}
