package api

import (
	"fmt"
	"strings"

	"github.com/crizzy9/plano/internal/feeds"
)

func lhrTerminal(value string) string {
	terminal := connectionCode(value)
	if suffix, ok := strings.CutPrefix(terminal, "TERMINAL"); ok {
		terminal = strings.TrimSpace(suffix)
	} else {
		terminal = strings.TrimSpace(strings.TrimPrefix(terminal, "T"))
	}
	switch terminal {
	case "2", "3", "4", "5":
		return terminal
	default:
		return ""
	}
}

// Heathrow publishes a separate process for through-ticketed connections and
// self-transfers. Both require screening before the onward flight.
func lhrConnectionDirections(in, out feeds.Flight) connectionGuideDirections {
	from, to := lhrTerminal(in.ArrivalTerminal), lhrTerminal(out.DepartureTerminal)
	through := "If both flights are on the same ticket, follow the purple Flight Connections signs."
	switch {
	case from == "" || to == "":
		through += " Check the connection screens for your onward terminal. Use the free shuttle bus if you need to change terminals."
	case from != to:
		through += fmt.Sprintf(" Take the free Flight Connections shuttle bus from Terminal %s to Terminal %s.", from, to)
	default:
		through += fmt.Sprintf(" Continue to connections security in Terminal %s.", to)
	}
	return connectionGuideDirections{requiresScreening: true, steps: []string{
		through,
		"If you are connecting on one ticket from abroad to a UK or Republic of Ireland flight, follow the signs for biometrics before security.",
		"If your flights are on separate tickets, follow Arrivals, clear any required immigration, collect any checked bags, and check in for your next flight.",
		"All passengers must clear security before boarding their onward flight at Heathrow.",
	}}
}
