package api

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

type yyzArea int

const (
	yyzAreaUnknown yyzArea = iota
	yyzAreaCanada
	yyzAreaUS
	yyzAreaInternational
)

var yyzICAOPattern = regexp.MustCompile(`^[A-Z]{4}$`)

func yyzAirportArea(primary, icao string) yyzArea {
	primary, icao = connectionCode(primary), connectionCode(icao)
	if icao == "" {
		icao = primary
	} else if yyzICAOPattern.MatchString(primary) && primary != icao {
		return yyzAreaUnknown
	}
	if !yyzICAOPattern.MatchString(icao) || icao == "ZZZZ" {
		return yyzAreaUnknown
	}
	// ICAO location-indicator allocations identify Canada and the U.S. states.
	// Leave island territories to Pearson's flight-specific connection guide.
	switch icao[:2] {
	case "CU", "CW", "CY", "CZ":
		return yyzAreaCanada
	case "PA", "PF", "PH", "PO", "PP":
		return yyzAreaUS
	case "PG", "PJ", "PL", "PM", "PW", "TI", "TJ", "TS", "NS":
		return yyzAreaUnknown
	}
	if icao[0] == 'K' {
		return yyzAreaUS
	}
	return yyzAreaInternational
}

func yyzGate(gate string) string {
	gate = strings.TrimSpace(gate)
	if len(gate) > 1 && gate[1] == ' ' {
		gate = gate[:1] + strings.TrimSpace(gate[1:])
	}
	return gate
}

func yyzGateSectors(gate string) string {
	// Pearson's maps give each swing gate a label for both operating areas.
	switch gate {
	case "D32", "F32", "D34", "F34", "D36", "F36", "D51", "F51", "D53", "F53", "D55", "F55", "D57", "F57":
		return "DF"
	case "E66", "F66", "E67", "F67", "E68", "F68", "E69", "F69":
		return "EF"
	case "A17", "B17", "A18", "B18", "A19", "B19", "A20", "B20":
		return "AB"
	default:
		return gateConcourse(gate, "ABCDEF")
	}
}

func yyzConnectionLocation(terminal, gate string) (string, string) {
	terminal = connectionCode(terminal)
	if terminal != "" {
		if suffix, ok := strings.CutPrefix(terminal, "TERMINAL"); ok {
			terminal = strings.TrimSpace(suffix)
		} else {
			terminal = strings.TrimSpace(strings.TrimPrefix(terminal, "T"))
		}
		if terminal != "1" && terminal != "3" {
			return "", ""
		}
	}
	gate = strings.TrimPrefix(connectionCode(gate), "GATE ")
	if gate == "" {
		return terminal, ""
	}
	first, second, paired := strings.Cut(gate, "/")
	first = yyzGate(first)
	sectors := yyzGateSectors(first)
	if sectors == "" {
		return "", ""
	}
	if paired {
		second = yyzGate(second)
		if len(sectors) != 2 || yyzGateSectors(second) != sectors || first[0] == second[0] || first[1:] != second[1:] {
			return "", ""
		}
	}
	fromGate := "1"
	if strings.ContainsAny(sectors, "ABC") {
		fromGate = "3"
	}
	if terminal != "" && terminal != fromGate {
		return "", ""
	}
	return fromGate, sectors
}

func yyzConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	from, arrivalSectors := yyzConnectionLocation(in.ArrivalTerminal, in.ArrivalGate)
	to, sectors := yyzConnectionLocation(out.DepartureTerminal, out.DepartureGate)
	if from == "" || to == "" {
		return connectionGuideDirections{}
	}
	arrivalArea := yyzAirportArea(in.Origin, in.OriginICAO)
	departureArea := yyzAirportArea(out.Destination, out.DestinationICAO)
	const domesticSectors = "DBC"
	if arrivalArea == yyzAreaCanada && arrivalSectors != "" && !strings.ContainsAny(arrivalSectors, domesticSectors) {
		return connectionGuideDirections{}
	}
	// The B and C gates serve both Canadian and non-U.S. international
	// departures. Even a single reported swing-gate label can be ambiguous.
	var departureSectors string
	switch departureArea {
	case yyzAreaCanada:
		departureSectors = domesticSectors
	case yyzAreaUS:
		departureSectors = "FA"
	case yyzAreaInternational:
		departureSectors = "EBC"
	}
	if sectors != "" && departureSectors != "" && !strings.ContainsAny(sectors, departureSectors) {
		return connectionGuideDirections{}
	}

	foreignArrival := arrivalArea == yyzAreaUS || arrivalArea == yyzAreaInternational
	changesTerminal := from != to
	terminal3US := to == "3" && departureArea == yyzAreaUS
	directions := connectionGuideDirections{
		landside:           changesTerminal || terminal3US,
		secureSameTerminal: !changesTerminal && arrivalArea == yyzAreaCanada && departureArea == yyzAreaCanada,
	}
	if changesTerminal {
		if foreignArrival {
			directions.steps = append(directions.steps, "Follow Arrivals through Canadian border control before taking Terminal Link.")
		}
		directions.steps = append(directions.steps,
			fmt.Sprintf("Take the Terminal Link train from Terminal %s to Terminal %s before security.", from, to),
			"Check in or recheck bags at the onward terminal if your airline directs you there.")
	} else if !terminal3US {
		directions.steps = append(directions.steps, fmt.Sprintf("Follow Connections signs within Terminal %s.", to))
	}

	directions.requiresBorderControl = departureArea == yyzAreaUS || foreignArrival && (directions.landside || departureArea == yyzAreaCanada)
	directions.requiresScreening = !directions.secureSameTerminal
	switch {
	case departureArea == yyzAreaUS:
		if foreignArrival && !changesTerminal {
			directions.steps = append(directions.steps, "Follow signs for Canadian border inspection if directed before continuing to U.S. screening and preclearance.")
		}
		if terminal3US {
			if !changesTerminal {
				directions.steps = append(directions.steps, "Check in or recheck bags if your airline directs you to.")
			}
			directions.steps = append(directions.steps, "Follow signs for A gates to the public departures entrance for security and U.S. preclearance.")
		} else {
			directions.steps = append(directions.steps, "Follow signs for F gates and allow time for security and U.S. preclearance before boarding.")
		}
	case directions.landside:
		directions.steps = append(directions.steps, "Go through departure screening in the onward terminal. Use Pearson's connection guide to check any border requirements for your flights.")
	case foreignArrival && departureArea == yyzAreaCanada:
		directions.steps = append(directions.steps, "Follow signs for Canadian border control and any required transfer screening before proceeding to your domestic gate.")
	case !directions.secureSameTerminal:
		directions.steps = append(directions.steps, "Use Pearson's connection guide to check which border checks and transfer screening apply to your flights.")
	}
	if !directions.landside {
		directions.steps = append(directions.steps, "Confirm with your airline whether your checked bags are transferred. If you need to collect bags or check in again, follow Arrivals and allow time for any required Canadian border control, baggage recheck and departure screening.")
	}
	directions.steps = append(directions.steps, "Check the airport screens for your onward gate.")
	return directions
}
