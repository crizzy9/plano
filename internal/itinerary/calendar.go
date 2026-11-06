package itinerary

import (
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	calendarDateTime       = "20060102T150405"
	calendarZoneCacheLimit = 32
)

var calendarZoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`)

// SubscriptionCalendar reads a bounded feed. Pass an inclusive YYYY-MM-DD
// cutoff to leave older events outside the leg limit, or an empty cutoff for history.
func SubscriptionCalendar(text, from string) (Itinerary, error) { return calendar(text, 1024, from) }

func calendar(text string, limit int, from string) (Itinerary, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\n ", ""), "\n\t", "")
	result := Itinerary{}
	seen := make(map[Leg]bool)
	eventSeen := make(map[Leg]bool)
	locations := make(map[string]*time.Location)
	var eventLegs []Leg
	var active, cancelled, recurring bool
	var start, startValue, startZone, summary, description string
	var nested int
	var locator string
	var conflicting bool
	for _, line := range strings.Split(text, "\n") {
		key, zone, value := calendarProperty(line)
		key = strings.ToUpper(key)
		if active && (nested > 0 || key == "BEGIN") {
			// Alarm and other child components have their own properties.
			switch key {
			case "BEGIN":
				nested++
			case "END":
				nested--
			}
			continue
		}
		switch key {
		case "BEGIN":
			if strings.EqualFold(value, "VEVENT") {
				active, cancelled, recurring = true, false, false
				start, startValue, startZone, summary, description = "", "", "", "", ""
			}
		case "END":
			if !strings.EqualFold(value, "VEVENT") || !active {
				continue
			}
			active = false
			if cancelled || recurring || start == "" || start < from {
				continue
			}
			departure := calendarDeparture(startValue, startZone, locations)
			clear(eventSeen)
			eventLegs = addLegs(eventLegs[:0], eventSeen, summary, start, departure, limit+1)
			if len(eventLegs) == 0 {
				eventLegs = addLegs(eventLegs, eventSeen, description, start, departure, limit+1)
			}
			if len(eventLegs) == 0 {
				continue
			}
			for _, leg := range eventLegs {
				if !seen[leg] && len(result.Legs) <= limit {
					result.Legs = append(result.Legs, leg)
					seen[leg] = true
				}
			}
			code := bookingCode(description)
			if code != "" && locator != "" && locator != code {
				conflicting = true
			}
			if code != "" {
				locator = code
			}
		case "DTSTART":
			if active && len(value) >= 8 {
				day, err := time.Parse("20060102", value[:8])
				if err == nil {
					start = day.Format(time.DateOnly)
					startValue, startZone = value, zone
				}
			}
		case "STATUS":
			if active && strings.EqualFold(value, "CANCELLED") {
				cancelled = true
			}
		case "RRULE", "RDATE", "RECURRENCE-ID":
			if active {
				recurring = true
			}
		case "SUMMARY":
			if active {
				summary = unescapeCalendar(value)
			}
		case "DESCRIPTION":
			if active {
				description = unescapeCalendar(value)
			}
		}
	}
	if !conflicting && len(result.Legs) == 1 {
		result.BookingCode = locator
	}
	return limitItinerary(result, limit), nil
}

// calendarProperty splits the content line outside quoted parameter values.
func calendarProperty(line string) (name, zone, value string) {
	start, quoted := 0, false
	for index, ch := range line {
		if ch == '"' {
			quoted = !quoted
		}
		if quoted || (ch != ';' && ch != ':') {
			continue
		}
		if start == 0 {
			name = line[:index]
		} else if key, parameter, ok := strings.Cut(line[start:index], "="); ok && strings.EqualFold(key, "TZID") {
			zone = strings.Trim(parameter, `"`)
		}
		if ch == ':' {
			return name, zone, line[index+1:]
		}
		start = index + 1
	}
	return "", "", ""
}

func calendarDeparture(value, zone string, locations map[string]*time.Location) time.Time {
	if departure, err := time.Parse(calendarDateTime+"Z", value); err == nil {
		return departure
	}
	if len(zone) > 128 || !calendarZoneName.MatchString(zone) || zone == "Local" || zone == "localtime" ||
		zone == "posixrules" || strings.HasPrefix(zone, "posix/") || strings.HasPrefix(zone, "right/") {
		return time.Time{}
	}
	departure, err := time.Parse(calendarDateTime, value)
	if err != nil {
		return time.Time{}
	}
	location, cached := locations[zone]
	if !cached {
		location, _ = time.LoadLocation(zone)
		if len(locations) < calendarZoneCacheLimit {
			locations[zone] = location
		}
	}
	if location == nil {
		return time.Time{}
	}
	local := time.Date(departure.Year(), departure.Month(), departure.Day(),
		departure.Hour(), departure.Minute(), departure.Second(), 0, location)
	start, end := local.ZoneBounds()
	for _, transition := range [2]time.Time{start, end} {
		if transition.IsZero() {
			continue
		}
		_, before := transition.Add(-time.Nanosecond).Zone()
		_, after := transition.Zone()
		beforeWall := transition.UTC().Add(time.Duration(before) * time.Second)
		afterWall := transition.UTC().Add(time.Duration(after) * time.Second)
		lower, upper := beforeWall, afterWall
		if upper.Before(lower) {
			lower, upper = upper, lower
		}
		if !departure.Before(lower) && departure.Before(upper) {
			// RFC 5545 uses the prior offset for a gap and the first instant for a fold.
			return departure.Add(-time.Duration(before) * time.Second)
		}
	}
	return local.UTC()
}

func unescapeCalendar(value string) string {
	return strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`).Replace(value)
}
