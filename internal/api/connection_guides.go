package api

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

type airportConnectionGuide struct {
	iata, icao, mapURL, guideURL string
	directions                   func(in, out feeds.Flight) connectionGuideDirections
}

type connectionGuideDirections struct {
	steps                 []string
	transferMinutes       int
	landside              bool
	requiresScreening     bool
	requiresBorderControl bool
	secureSameTerminal    bool
}

func secureConnectionDirections(directions func(in, out feeds.Flight) []string) func(feeds.Flight, feeds.Flight) connectionGuideDirections {
	return func(in, out feeds.Flight) connectionGuideDirections {
		return connectionGuideDirections{steps: directions(in, out)}
	}
}

var airportConnectionGuides = []airportConnectionGuide{
	{"SEA", "KSEA", "https://www.portseattle.org/sea/maps", "https://www.portseattle.org/blog/all-aboard-sea-underground",
		secureConnectionDirections(func(in, out feeds.Flight) []string { return seaConnectionDirections(in.ArrivalGate, out.DepartureGate) })},
	{"DFW", "KDFW", "https://www.dfwairport.com/map/", "https://www.dfwairport.com/explore/plan/connect/", secureConnectionDirections(dfwConnectionDirections)},
	{"ATL", "KATL", "https://www.atl.com/maps/", "https://www.atl.com/maps-3/", secureConnectionDirections(atlConnectionDirections)},
	{"SFO", "KSFO", "https://www.flysfo.com/maps", "https://www.flysfo.com/connecting-flights-faq", secureConnectionDirections(sfoConnectionDirections)},
	{"EWR", "KEWR", "https://www.newarkairport.com/explore-ewr/airport-map", "https://www.newarkairport.com/flights/connections-guide", ewrConnectionDirections},
	{"JFK", "KJFK", "https://www.jfkairport.com/explore-jfk/airport-map", "https://www.jfkairport.com/flights/connections-guide", jfkConnectionDirections},
	{"LAX", "KLAX", "https://www.flylax.com/lax-terminal-maps", "https://www.flylax.com/sites/lax/files/2025-12/Terminal%20Connections%20Pedestrian%20Path%2025-1222.pdf", secureConnectionDirections(laxConnectionDirections)},
	{"ORD", "KORD", "https://maps.ord.flychicago.com/", "https://www.flychicago.com/ohare/tofrom/connecting/Pages/default.aspx", ordConnectionDirections},
	{"DEN", "KDEN", "https://maps.flydenver.com/", "https://www.flydenver.com/about-den/facilities/#train", denConnectionDirections},
	{"LHR", "EGLL", "https://www.heathrow.com/at-the-airport/airport-maps", "https://www.heathrow.com/connecting-flights", lhrConnectionDirections},
	{"AMS", "EHAM", "https://www.schiphol.nl/en/airport-maps/", "https://www.schiphol.nl/en/transfers/", amsConnectionDirections},
	{"CDG", "LFPG", "https://www.parisaeroport.fr/en/passengers/at-the-airport/terminals-map/cdg", "https://wwws.airfrance.us/information/aeroport/conseils-correspondances-paris-cdg-ory", cdgConnectionDirections},
	{"FRA", "EDDF", "https://www.frankfurt-airport.com/en/at-the-airport/orientation/airport-map.html", "https://www.frankfurt-airport.com/en/flights-and-transfer/transferring-at-fra.html", fraConnectionDirections},
	{"SIN", "WSSS", "https://www.changiairport.com/en/at-changi/map.html", "https://www.changiairport.com/en/fly/transit-guide.html", sinConnectionDirections},
	{"YYZ", "CYYZ", "https://www.torontopearson.com/en/while-you-are-here/terminal-maps", "https://www.torontopearson.com/en/connections", yyzConnectionDirections},
	{"HND", "RJTT", "https://tokyo-haneda.com/en/floor/index.html", "https://tokyo-haneda.com/en/flight/transit/index.html", hndConnectionDirections},
}

func (g airportConnectionGuide) matches(primary, icao string) bool {
	primary, icao = connectionCode(primary), connectionCode(icao)
	return (primary != "" || icao != "") && (primary == "" || primary == g.iata || primary == g.icao) &&
		(icao == "" || icao == g.icao)
}

func (r *ConnectionRules) routeForConnection(in, out feeds.Flight) (ConnectionRoute, bool) {
	graph := r.graphForAirport(in.Destination, in.DestinationICAO)
	route, configured := graph.route(in.ArrivalTerminal, in.ArrivalGate, out.DepartureTerminal, out.DepartureGate)
	for _, guide := range airportConnectionGuides {
		if !guide.matches(in.Destination, in.DestinationICAO) || !guide.matches(out.Origin, out.OriginICAO) {
			continue
		}
		if route.MapURL == "" {
			route.MapURL = guide.mapURL
		}
		// Operator checkpoints can account for local restrictions. If their
		// graph has no applicable path, keep the general directions.
		if !configured && (graph == nil || len(graph.nodes) == 0) && guide.directions != nil && !connectionDisrupted(in, out) {
			if directions := guide.directions(in, out); len(directions.steps) != 0 {
				switch {
				case directions.landside, directions.requiresScreening, directions.requiresBorderControl:
					route.TransferMinutes = defaultConnectionTerminalTransferMinutes
				case directions.secureSameTerminal && connectionCode(in.ArrivalTerminal) != "" && connectionCode(out.DepartureTerminal) != "":
					route.TransferMinutes = defaultConnectionSameTerminalMinutes
				}
				route.TransferMinutes = max(route.TransferMinutes, directions.transferMinutes)
				// Keep the planning time for scoring after departure.
				if !connectionDeparted(out) {
					route.Instructions = directions.steps
					switch {
					case directions.landside:
						route.Instructions = append([]string{"Complete any required passport control and collect checked bags as directed. Check where to recheck them before going through security."}, directions.steps...)
					case !directions.requiresScreening && !directions.requiresBorderControl:
						route.Instructions = append([]string{"Use these directions if you can stay in the secure area from your arrival gate."}, directions.steps...)
					}
					route.GuideURL = guide.guideURL
				}
			}
		}
		break
	}
	return route, configured
}

var guideGatePattern = regexp.MustCompile(`^([A-Z])(0?[1-9]|[1-9][0-9])[A-Z]?$`)

func gateConcourse(gate, allowed string) string {
	match := guideGatePattern.FindStringSubmatch(connectionCode(gate))
	if match == nil || !strings.Contains(allowed, match[1]) {
		return ""
	}
	return match[1]
}

func dfwTerminal(terminal, gate string) string {
	terminal = strings.TrimPrefix(connectionCode(terminal), "TERMINAL ")
	gateTerminal := gateConcourse(gate, "ABCDE")
	if connectionCode(gate) != "" && gateTerminal == "" {
		return ""
	}
	if terminal == "" {
		return gateTerminal
	}
	if len(terminal) != 1 || !strings.Contains("ABCDE", terminal) || gateTerminal != "" && gateTerminal != terminal {
		return ""
	}
	return terminal
}

func dfwConnectionDirections(in, out feeds.Flight) []string {
	from, to := dfwTerminal(in.ArrivalTerminal, in.ArrivalGate), dfwTerminal(out.DepartureTerminal, out.DepartureGate)
	if from == "" || to == "" {
		return nil
	}
	if from == to {
		return []string{fmt.Sprintf("Follow departure gate signs within Terminal %s.", to)}
	}
	return []string{fmt.Sprintf("Follow Skylink signs in Terminal %s and check the platform signs for a train serving Terminal %s.", from, to)}
}

func atlConnectionDirections(in, out feeds.Flight) []string {
	from, to := gateConcourse(in.ArrivalGate, "TABCDEF"), gateConcourse(out.DepartureGate, "TABCDEF")
	if from == "" || to == "" {
		return nil
	}
	if from == to {
		return []string{fmt.Sprintf("Follow departure gate signs within Concourse %s.", to)}
	}
	return []string{fmt.Sprintf("Follow signs to the Plane Train or the pedestrian walkway for Concourse %s.", to)}
}
