package itinerary

import (
	"strings"
	"time"
)

func validDepartureZone(zone string) bool {
	return len(zone) <= 128 && calendarZoneName.MatchString(zone) && zone != "Local" && zone != "localtime" &&
		zone != "posixrules" && !strings.HasPrefix(zone, "posix/") && !strings.HasPrefix(zone, "right/")
}

// MatchesLocalDeparture compares a confirmation's wall-clock minute at the
// scheduled departure airport. Unknown zones cannot resolve a local clock.
func (leg Leg) MatchesLocalDeparture(departure time.Time, zone string) bool {
	if leg.LocalDeparture == "" || !validDepartureZone(zone) {
		return false
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return false
	}
	local := departure.In(location)
	return local.Format(time.DateOnly) == leg.Date && local.Format("15:04") == leg.LocalDeparture
}
