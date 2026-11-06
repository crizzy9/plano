package itinerary

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	calendarDateTime       = "20060102T150405"
	calendarUTCDateTime    = calendarDateTime + "Z"
	calendarZoneCacheLimit = 32
)

var calendarZoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`)

// SubscriptionCalendar reads a bounded feed. Pass an inclusive YYYY-MM-DD
// cutoff to leave older events outside the leg limit, or an empty cutoff for history.
func SubscriptionCalendar(text, from string) (Itinerary, error) {
	return calendar(text, 1024, from)
}

func calendar(text string, limit int, from string) (Itinerary, error) {
	input := readCalendar(text, false)
	var cancellations calendarCancellations
	input.collectCancellations(&cancellations)
	return input.confirmed(limit, from, &cancellations, nil), nil
}

type calendarInput struct {
	lines                          []string
	cancelled                      []calendarRange
	forceCancel, hasCancelledEvent bool
}

type calendarEvent struct {
	uid, start, zone, recurrence, recurrenceZone, summary, description string
	calendarRevision
	cancelled, recurring bool
}
type calendarRevision struct {
	sequence int
	stamp    string // validated UTC DTSTAMP, ordered lexically
}

func (r calendarRevision) after(other calendarRevision) bool {
	return r.sequence > other.sequence || r.sequence == other.sequence && r.stamp > other.stamp
}

type calendarRange struct{ start, end int }

// METHOD belongs to the calendar object and can occur after its events. Scan
// for cancelled objects before extracting legs so property order has no effect.
func readCalendar(text string, forceCancel bool) calendarInput {
	text = strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\n ", ""), "\n\t", "")
	input := calendarInput{lines: strings.Split(text, "\n"), forceCancel: forceCancel}
	depth, start, cancelled := 0, -1, false
	for index, line := range input.lines {
		key, _, value := calendarProperty(line)
		switch strings.ToUpper(key) {
		case "BEGIN":
			if depth == 0 && strings.EqualFold(value, "VCALENDAR") {
				start, cancelled = index, false
			}
			depth++
		case "END":
			if depth > 0 {
				depth--
			}
			if depth == 0 && start >= 0 {
				if cancelled {
					input.cancelled = append(input.cancelled, calendarRange{start, index})
				}
				start = -1
			}
		case "METHOD":
			if depth == 1 && start >= 0 && strings.EqualFold(strings.TrimSpace(value), "CANCEL") {
				cancelled = true
			}
		case "STATUS":
			input.hasCancelledEvent = input.hasCancelledEvent || strings.EqualFold(value, "CANCELLED")
		}
	}
	if start >= 0 && cancelled {
		input.cancelled = append(input.cancelled, calendarRange{start, len(input.lines) - 1})
	}
	return input
}

func (input calendarInput) eachEvent(visit func(calendarEvent)) {
	var event calendarEvent
	var active bool
	var nested int
	cancelled := input.cancelled
	for index, line := range input.lines {
		if len(cancelled) > 0 && index > cancelled[0].end {
			cancelled = cancelled[1:]
		}
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
		if key == "BEGIN" && strings.EqualFold(value, "VEVENT") {
			active = true
			event = calendarEvent{}
			continue
		}
		if !active {
			continue
		}
		switch key {
		case "END":
			if strings.EqualFold(value, "VEVENT") {
				active = false
				event.cancelled = event.cancelled || input.forceCancel || len(cancelled) > 0 && index >= cancelled[0].start
				visit(event)
			}
		case "DTSTART":
			if calendarDay(value) != "" {
				event.start, event.zone = value, zone
			}
		case "STATUS":
			event.cancelled = event.cancelled || strings.EqualFold(value, "CANCELLED")
		case "RECURRENCE-ID":
			event.recurring = true
			event.recurrence, event.recurrenceZone = value, zone
		case "RRULE", "RDATE":
			event.recurring = true
		case "SUMMARY":
			event.summary = value
		case "DESCRIPTION":
			event.description = value
		case "UID":
			event.uid = value
		case "SEQUENCE":
			if sequence, err := strconv.Atoi(value); err == nil && sequence >= 0 {
				event.sequence = sequence
			}
		case "DTSTAMP":
			if zone == "" && len(value) == len(calendarUTCDateTime) {
				if _, err := time.Parse(calendarUTCDateTime, value); err == nil {
					event.stamp = value
				}
			}
		}
	}
}

// Rank master events before filtering dates and extracting flight legs. A
// newer revision can move an event outside the window or make it recurring.
func (input calendarInput) latestConfirmedRevisions() map[string]calendarRevision {
	latest := make(map[string]calendarRevision)
	input.eachEvent(func(event calendarEvent) {
		if event.uid == "" || event.cancelled || event.recurrence != "" {
			return
		}
		previous, found := latest[event.uid]
		if !found || event.calendarRevision.after(previous) {
			latest[event.uid] = event.calendarRevision
		}
	})
	return latest
}

func (event calendarEvent) flightLegs(legs []Leg, seen map[Leg]bool, start string, departure time.Time, limit int) []Leg {
	clear(seen)
	legs = addLegs(legs[:0], seen, unescapeCalendar(event.summary), start, departure, limit+1, nil)
	if len(legs) == 0 {
		legs = addLegs(legs, seen, unescapeCalendar(event.description), start, departure, limit+1, nil)
	}
	return legs
}

func (input calendarInput) confirmed(limit int, from string, cancellations *calendarCancellations, localDepartures map[Leg]bool) Itinerary {
	result := Itinerary{}
	seen := make(map[Leg]bool)
	eventSeen := make(map[Leg]bool)
	locations := make(map[string]*time.Location)
	latest := input.latestConfirmedRevisions()
	var eventLegs []Leg
	var locator string
	var conflicting bool
	input.eachEvent(func(event calendarEvent) {
		if event.cancelled || event.recurring {
			return
		}
		cancelledUID := cancellations.matchesUID(event)
		if cancelledUID && localDepartures == nil {
			return
		}
		if revision, found := latest[event.uid]; found && !cancelledUID && revision.after(event.calendarRevision) {
			return
		}
		start := calendarDay(event.start)
		if start == "" || start < from {
			return
		}
		departure := calendarDeparture(event.start, event.zone, locations)
		eventLegs = event.flightLegs(eventLegs, eventSeen, start, departure, limit)
		var accepted bool
		for _, leg := range eventLegs {
			if cancelledUID {
				cancellations.recordText(event, leg)
				continue
			}
			if cancellations.matchesSchedule(event, leg) || cancellations.matchesNotice(event, leg) {
				continue
			}
			accepted = true
			if !seen[leg] && len(result.Legs) <= limit {
				result.Legs = append(result.Legs, leg)
				seen[leg] = true
			}
			if localDepartures != nil && seen[leg] && !departure.IsZero() && len(event.start) == len(calendarDateTime) {
				// A recognized TZID preserves the calendar event's wall clock.
				clock := event.start[9:11] + ":" + event.start[11:13]
				localDepartures[Leg{Number: leg.Number, Date: leg.Date, LocalDeparture: clock}] = true
			}
		}
		if accepted {
			code := bookingCode(unescapeCalendar(event.description))
			if code != "" && locator != "" && locator != code {
				conflicting = true
			}
			if code != "" {
				locator = code
			}
		}
	})
	if !conflicting && len(result.Legs) == 1 {
		result.BookingCode = locator
	}
	return limitItinerary(result, limit)
}

type calendarCancellations struct {
	recognized bool
	revisions  map[string]calendarRevision
	schedules  map[Leg]bool // true when a cancellation for this schedule omitted UID
	textLegs   map[Leg]bool
	notices    emailCancellationNotices
}

func (input calendarInput) collectCancellations(cancellations *calendarCancellations) bool {
	if !input.forceCancel && !input.hasCancelledEvent && len(input.cancelled) == 0 {
		return false
	}
	var recognized bool
	eventSeen := make(map[Leg]bool)
	locations := make(map[string]*time.Location)
	var eventLegs []Leg
	input.eachEvent(func(event calendarEvent) {
		if !event.cancelled {
			return
		}
		recognized = true
		if !cancellations.recognized {
			cancellations.recognized = true
			cancellations.revisions = make(map[string]calendarRevision)
			cancellations.schedules = make(map[Leg]bool)
			cancellations.textLegs = make(map[Leg]bool)
		}
		// A recurrence-specific cancellation does not cancel a whole event UID.
		if event.uid != "" && event.recurrence == "" {
			previous, found := cancellations.revisions[event.uid]
			// Keep an unknown timestamp at the highest sequence. It can only
			// be superseded by a confirmation at a higher sequence.
			if !found || event.sequence > previous.sequence ||
				event.sequence == previous.sequence && previous.stamp != "" && (event.stamp == "" || event.calendarRevision.after(previous)) {
				cancellations.revisions[event.uid] = event.calendarRevision
			}
		}
		if event.start == "" {
			event.start, event.zone = event.recurrence, event.recurrenceZone
		}
		start := calendarDay(event.start)
		if start == "" {
			return
		}
		departure := calendarDeparture(event.start, event.zone, locations)
		eventLegs = event.flightLegs(eventLegs, eventSeen, start, departure, MaxFlights)
		for _, leg := range eventLegs {
			if event.recurrence == "" {
				schedule := calendarSchedule(event, leg)
				cancellations.schedules[schedule] = cancellations.schedules[schedule] || event.uid == ""
			}
			cancellations.recordText(event, leg)
		}
	})
	return recognized
}

func (cancellations *calendarCancellations) matchesUID(event calendarEvent) bool {
	revision, found := cancellations.revisions[event.uid]
	if !found || revision.sequence != event.sequence {
		return found && revision.sequence > event.sequence
	}
	return revision.stamp == "" || !event.calendarRevision.after(revision)
}

func (cancellations *calendarCancellations) matchesSchedule(event calendarEvent, leg Leg) bool {
	if len(cancellations.schedules) == 0 {
		return false
	}
	anonymous, ok := cancellations.schedules[calendarSchedule(event, leg)]
	return ok && (anonymous || event.uid == "")
}

func (cancellations *calendarCancellations) matchesText(leg Leg) bool {
	return cancellations.textLegs[cancellationLegKey(leg)] || cancellations.notices.matches(leg)
}

func (cancellations *calendarCancellations) matchesNotice(event calendarEvent, leg Leg) bool {
	if len(cancellations.notices) == 0 {
		return false
	}
	clock := calendarClock(event.start)
	if leg.UTCDeparture.IsZero() {
		leg.LocalDeparture = clock
		return cancellations.notices.matches(leg)
	}
	return cancellations.notices.matches(leg) || clock != "" &&
		cancellations.notices.matches(Leg{Number: leg.Number, Date: leg.Date, LocalDeparture: clock})
}

func cancellationLegKey(leg Leg) Leg {
	if !leg.UTCDeparture.IsZero() {
		leg = Leg{Number: leg.Number, UTCDeparture: leg.UTCDeparture.UTC().Truncate(time.Minute)}
	}
	return leg
}

func (cancellations *calendarCancellations) recordText(event calendarEvent, leg Leg) {
	cancellations.textLegs[Leg{Number: leg.Number, Date: leg.Date}] = true
	if !leg.UTCDeparture.IsZero() {
		instant := leg.UTCDeparture.UTC().Truncate(time.Minute)
		cancellations.textLegs[Leg{Number: leg.Number, UTCDeparture: instant}] = true
	}
	if clock := calendarClock(event.start); clock != "" {
		cancellations.textLegs[Leg{Number: leg.Number, Date: leg.Date, LocalDeparture: clock}] = true
	}
}

func calendarSchedule(event calendarEvent, leg Leg) Leg {
	if !leg.UTCDeparture.IsZero() {
		return Leg{Number: leg.Number, UTCDeparture: leg.UTCDeparture.UTC()}
	}
	leg.LocalDeparture = calendarClock(event.start)
	return leg
}

func calendarClock(start string) string {
	if wall, err := time.Parse(calendarDateTime, start); err == nil {
		return wall.Format("15:04")
	}
	return ""
}

func calendarDay(value string) string {
	if len(value) >= 8 {
		if day, err := time.Parse("20060102", value[:8]); err == nil {
			return day.Format(time.DateOnly)
		}
	}
	return ""
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
	if departure, err := time.Parse(calendarUTCDateTime, value); err == nil {
		return departure
	}
	if !validDepartureZone(zone) {
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
