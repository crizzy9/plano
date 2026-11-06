package api

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

type hndFlightArea string

const (
	hndDomestic      hndFlightArea = "domestic"
	hndInternational hndFlightArea = "international"
)

var (
	hndICAOPattern = regexp.MustCompile(`^[A-HK-PR-WYZ][A-Z]{3}$`)
	hndGatePattern = regexp.MustCompile(`^(?:GATE\s*)?([0-9]{1,3})[AB]?$`)
)

func hndConnectionEndpoint(terminal, gate, remoteICAO string) (string, hndFlightArea) {
	terminal = connectionCode(terminal)
	if suffix, ok := strings.CutPrefix(terminal, "TERMINAL"); ok {
		terminal = strings.TrimSpace(suffix)
	} else {
		terminal = strings.TrimSpace(strings.TrimPrefix(terminal, "T"))
	}

	var area hndFlightArea
	switch terminal {
	case "1":
		area = hndDomestic
	case "2":
		// Terminal 2 has both domestic and international flights.
	case "3":
		area = hndInternational
	default:
		return "", ""
	}

	// The Japan Meteorological Agency's aerodrome indicators use RJ and RO.
	// Treat unassigned and filed-in-flight indicators as missing geography.
	remoteICAO = connectionCode(remoteICAO)
	if hndICAOPattern.MatchString(remoteICAO) && remoteICAO != "ZZZZ" && remoteICAO != "AFIL" {
		remoteArea := hndInternational
		if strings.HasPrefix(remoteICAO, "RJ") || strings.HasPrefix(remoteICAO, "RO") {
			remoteArea = hndDomestic
		}
		if area != "" && remoteArea != area {
			return "", ""
		}
		area = remoteArea
	}
	gateTerminal, gateArea := hndConnectionGate(gate)
	if gateTerminal != "" && gateTerminal != terminal || gateArea != "" && area != "" && gateArea != area {
		return "", ""
	}
	if area == "" {
		area = gateArea
	}
	return terminal, area
}

// Use published boarding gate groups to check reported terminals. Terminal 2
// gates 66–69 can serve either type of flight. A gate alone cannot place a flight
// in a terminal because the feed may omit or change its assignments.
func hndConnectionGate(gate string) (string, hndFlightArea) {
	match := hndGatePattern.FindStringSubmatch(connectionCode(gate))
	if match == nil {
		return "", ""
	}
	number, _ := strconv.Atoi(match[1])
	switch {
	case number >= 1 && number <= 24, number >= 30 && number <= 40, number >= 84 && number <= 91:
		return "1", hndDomestic
	case number >= 47 && number <= 65, number >= 500 && number <= 513:
		return "2", hndDomestic
	case number >= 66 && number <= 69:
		return "2", ""
	case number >= 70 && number <= 73, number >= 700 && number <= 702:
		return "2", hndInternational
	case number >= 105 && number <= 149:
		return "3", hndInternational
	default:
		return "", ""
	}
}

func hndPublicShuttle(from, to string) string {
	stop := "0"
	switch from {
	case "1":
		stop = "8"
	case "2":
		stop = "9"
	}
	return fmt.Sprintf("Take the free public terminal shuttle from Bus Stop %s on the first floor of Terminal %s. Check the route signs for Terminal %s.", stop, from, to)
}

func hndConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	from, arriving := hndConnectionEndpoint(in.ArrivalTerminal, in.ArrivalGate, in.OriginICAO)
	to, departing := hndConnectionEndpoint(out.DepartureTerminal, out.DepartureGate, out.DestinationICAO)
	if arriving == "" || departing == "" {
		return connectionGuideDirections{}
	}
	const domesticDeparture = "Go to domestic departures on the second floor, check in or recheck bags if needed, and clear domestic security."
	const internationalDeparture = "Follow international departure signs, complete check-in or bag drop if needed, and clear security and departure passport control."
	const domesticBaggage = "Before leaving the arrival area, ask your airline whether you need to collect or recheck any bags."
	route := connectionGuideDirections{requiresScreening: true}
	switch {
	case arriving == hndDomestic && departing == hndDomestic:
		if from == to {
			return connectionGuideDirections{secureSameTerminal: true, steps: []string{
				"Follow Domestic Connecting Flights signs to the transfer facility in the arrival concourse. Confirm your boarding pass and baggage arrangements with the airline, then follow signs to your departure gate.",
			}}
		}
		route.steps = []string{domesticBaggage, hndPublicShuttle(from, to), domesticDeparture}
	case arriving == hndDomestic && departing == hndInternational:
		switch {
		case from == to:
			route.steps = []string{
				"If your airline confirms you can use the transfer facility, follow International Flight Transfer signs for Terminal 2. If you need to collect bags or use public check-in, go from domestic arrivals on the first floor to international departures on the third floor.",
				internationalDeparture,
			}
		case to == "3":
			sign := "International Flight Transfer"
			if from == "1" {
				sign = "International Transit"
			}
			route.steps = []string{
				fmt.Sprintf("If your airline confirms you can use the connecting bus, follow %s signs inside Terminal %s and board the dedicated bus to Terminal 3.", sign, from),
				"If you need to collect bags or the connecting bus is unavailable, use domestic arrivals. " + hndPublicShuttle(from, to),
				internationalDeparture,
			}
		default:
			route.steps = []string{domesticBaggage, hndPublicShuttle(from, to), internationalDeparture}
		}
	case departing == hndDomestic:
		route.requiresBorderControl = true
		route.steps = []string{"Complete Japanese immigration, collect any checked bags, and go through customs before transferring to your domestic flight."}
		switch {
		case from == to:
			route.steps = append(route.steps, "Follow signs from international arrivals to domestic departures on the second floor of Terminal 2. Check in or recheck bags if needed, then clear domestic security.")
		case from == "3":
			route.steps = append(route.steps,
				fmt.Sprintf("If your airline confirms the Terminal 3 domestic transfer counter and domestic transfer security checkpoint are available to you, use the counter on the second floor, clear domestic transfer security, then board the dedicated transfer bus to Terminal %s.", to),
				"If that transfer is unavailable, use the public shuttle instead. "+hndPublicShuttle(from, to)+" "+domesticDeparture)
		default:
			route.steps = append(route.steps, hndPublicShuttle(from, to), domesticDeparture)
		}
	default:
		if from == to {
			route.steps = []string{fmt.Sprintf("If your airline confirms you can stay in transit, follow International Transit signs within Terminal %s.", to)}
		} else {
			route.steps = []string{fmt.Sprintf("If your airline confirms you can stay in transit, follow International Transit signs to the dedicated bus to Terminal %s.", to)}
		}
		route.steps = append(route.steps, "Follow staff directions to clear international transit security. Ask at the transit counter if you need an onward boarding pass.")
		fallback := "If you need to enter Japan to collect bags or check in, confirm your eligibility to enter with your airline before leaving the transit area. Complete immigration and customs before going to public departures."
		if from != to {
			fallback += " " + hndPublicShuttle(from, to)
		}
		route.steps = append(route.steps, fallback+" "+internationalDeparture)
	}
	return route
}
