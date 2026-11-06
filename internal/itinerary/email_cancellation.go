package itinerary

import (
	"regexp"
	"strings"
)

var (
	emailCancellationPrefix = regexp.MustCompile(`(?i)^(?:flight cancellation|cancel(?:l)?ed)[ \t]*:[ \t]*(?:flight[ \t]+)?(.+)$`)
	emailCancellationNotice = regexp.MustCompile(`(?i)^(?:your[ \t]+)?flight[ \t]+(.+?)[ \t]+(?:has[ \t]+been|was|is)[ \t]+cancel(?:l)?ed(?:[ \t]+(?:due[ \t]+to|because)[ \t]+[^.!]+)?(?:[.!](?:[ \t]+(.*))?)?$`)
	emailCancellationStatus = regexp.MustCompile(`(?i)^[ \t]*(?:has[ \t]+been|was|is)[ \t]+cancel(?:l)?ed\b`)
	emailForwardPrefix      = regexp.MustCompile(`(?i)^(?:(?:subject|fwd?)[ \t]*:[ \t]*)+`)
	emailFlightDate         = regexp.MustCompile(`(?i)^on[ \t]+`)
)

// A date-only key is present for every cancellation. Its value says whether
// the notice omitted the time, leaving every departure on that date ambiguous.
type emailCancellationNotices map[Leg]bool

type emailCancellation struct {
	leg                  Leg
	wallClock, remainder string
}

func (notices emailCancellationNotices) strip(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range lines {
		notice, ok := cancelledEmailLeg(line)
		if !ok && i+1 < len(lines) && emailCancellationStatus.MatchString(lines[i+1]) {
			notice, ok = cancelledEmailLeg(strings.TrimSpace(line) + " " + strings.TrimSpace(lines[i+1]))
			if ok {
				lines[i+1] = ""
			}
		}
		if !ok {
			continue
		}
		leg := notice.leg
		date := Leg{Number: leg.Number, Date: leg.Date}
		notices[date] = notices[date] || leg.LocalDeparture == "" && leg.UTCDeparture.IsZero()
		if leg != date {
			notices[cancellationLegKey(leg)] = true
		}
		if notice.wallClock != "" {
			notices[Leg{Number: leg.Number, Date: leg.Date, LocalDeparture: notice.wallClock}] = true
		}
		lines[i] = notice.remainder
	}
	return strings.Join(lines, "\n")
}

func (notices emailCancellationNotices) matches(leg Leg) bool {
	anyTime, dated := notices[Leg{Number: leg.Number, Date: leg.Date}]
	if leg.LocalDeparture == "" && leg.UTCDeparture.IsZero() {
		return dated
	}
	return anyTime || notices[cancellationLegKey(leg)]
}

func cancelledEmailLeg(line string) (emailCancellation, bool) {
	line = strings.TrimSpace(strings.TrimLeft(line, " \t>"))
	line = emailForwardPrefix.ReplaceAllString(line, "")
	var description, remainder string
	if match := emailCancellationNotice.FindStringSubmatch(line); match != nil {
		description, remainder = match[1], match[2]
	} else if match := emailCancellationPrefix.FindStringSubmatch(line); match != nil {
		description = strings.TrimRight(match[1], ".!")
	} else {
		return emailCancellation{}, false
	}
	upper := strings.ToUpper(description)
	designator := flightPattern.FindStringSubmatchIndex(upper)
	if designator == nil || designator[0] != 0 {
		return emailCancellation{}, false
	}
	detail := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(description[designator[1]:]), "|"))
	detail = emailFlightDate.ReplaceAllString(detail, "")
	if schedule := splitScheduleLine(detail); schedule.labeled() {
		if schedule.prefix != "" || schedule.arrival || len(schedule.departures) != 1 {
			return emailCancellation{}, false
		}
		detail = schedule.departures[0].value
	}
	var clock string
	if zonedStart.MatchString(detail) {
		// A numeric offset preserves the wall time written in the notice.
		if !strings.HasSuffix(detail, "Z") {
			clock = detail[11:16]
		}
	} else {
		date := isoPattern.FindStringIndex(detail)
		if date == nil {
			date = wordDate.FindStringIndex(detail)
		}
		if date == nil || date[0] != 0 {
			return emailCancellation{}, false
		}
		if clock := strings.TrimSpace(detail[date[1]:]); clock != "" && !clockField.MatchString(clock) {
			return emailCancellation{}, false
		}
	}
	leg, ok := parseLabeledDeparture([]departureField{{value: detail}}, "")
	leg.Number = upper[designator[2]:designator[3]] + upper[designator[4]:designator[5]]
	return emailCancellation{leg: leg, wallClock: clock, remainder: remainder}, ok
}
