// Package itinerary extracts flight designators and departure dates from import inputs.
package itinerary

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

const MaxFlights = 8

var ErrInvalid = errors.New("no unambiguous dated flights in itinerary")

type Leg struct {
	Number, Date   string
	Origin         string
	Destination    string
	LocalDeparture string
	UTCDeparture   time.Time
}

type Itinerary struct {
	Legs             []Leg
	BookingCode      string
	Truncated        bool
	CancellationOnly bool
}

var (
	flightPattern = regexp.MustCompile(`\b([A-Z][A-Z0-9]{1,2}|[0-9][A-Z])[ -]?([0-9]{1,5}[A-Z]?)\b`)
	isoPattern    = regexp.MustCompile(`\b[0-9]{4}-[0-9]{2}-[0-9]{2}\b`)
	wordDate      = regexp.MustCompile(`(?i)\b(?:[0-9]{1,2} (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*|(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]* [0-9]{1,2}),? [0-9]{4}\b`)
	booking       = regexp.MustCompile(`(?im)\b(?:booking reference|record locator|reservation code|PNR|confirmation(?: number| code|[ \t]*#))\s*[:#-]?\s*([a-z0-9]{6})\b`)
)

func Parse(text, date string) (Itinerary, error) {
	if date != "" && !validDate(date) {
		return Itinerary{}, ErrInvalid
	}
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(text)), "BEGIN:VCALENDAR") {
		return calendar(text, MaxFlights, "")
	}
	notices := make(emailCancellationNotices)
	result := fromText(notices.strip(text), date, notices.matches)
	result.CancellationOnly = len(notices) != 0 && len(result.Legs) == 0
	if len(notices) != 0 {
		result.BookingCode = ""
	}
	return result, nil
}

func validDate(value string) bool {
	_, err := time.Parse(time.DateOnly, value)
	return err == nil
}

func fromText(text, date string, skip func(Leg) bool) Itinerary {
	result := Itinerary{BookingCode: bookingCode(text)}
	text = booking.ReplaceAllString(text, " ")
	var seen = make(map[Leg]bool)
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	records := labeledTextRecords(lines, date)
	// A suppressed precise leg still takes precedence over its own date-only
	// copy, including when the cancellation names the instant in UTC.
	skippedPreciseDays := make(map[Leg]bool)
	for _, record := range records {
		leg := record.leg
		if (leg.LocalDeparture != "" || !leg.UTCDeparture.IsZero()) && skip(leg) {
			skippedPreciseDays[Leg{Number: leg.Number, Date: leg.Date}] = true
		}
	}
	rejectLeg := func(leg Leg) bool { return skip(leg) || skippedPreciseDays[leg] }
	globalDate := date
	if globalDate == "" {
		globalDate = soleUnlabeledDate(lines, records)
	}
	// A date-only duplicate can coexist temporarily with a more precise leg.
	// Keep one extra candidate beyond two variants per permitted leg.
	const candidateLimit = 2*MaxFlights + 1
	var blockDate string
	for index, line := range lines {
		if record, labeled := records[index]; labeled {
			blockDate = record.heading
			globalDate = date
			if leg := record.leg; leg.Number != "" && !seen[leg] && !rejectLeg(leg) {
				result.Legs = append(result.Legs, leg)
				seen[leg] = true
			}
			if len(result.Legs) == candidateLimit {
				break
			}
			continue
		}
		lineDates := dates(line)
		if len(lineDates) > 1 {
			blockDate = ""
			continue
		}
		if len(lineDates) == 1 {
			blockDate = lineDates[0]
		}
		day := globalDate
		if len(lineDates) == 1 || day == "" {
			day = blockDate
		}
		if day == "" {
			continue
		}
		result.Legs = addLegs(result.Legs, seen, line, day, time.Time{}, candidateLimit, rejectLeg)
		if len(result.Legs) == candidateLimit {
			break
		}
	}
	result.Legs = preferPreciseLegs(result.Legs)
	return limitItinerary(result, MaxFlights)
}

func dates(text string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(value string) {
		if !seen[value] && validDate(value) {
			out = append(out, value)
			seen[value] = true
		}
	}
	for _, value := range isoPattern.FindAllString(text, -1) {
		add(value)
	}
	for _, value := range wordDate.FindAllString(text, -1) {
		value = strings.ReplaceAll(value, ",", "")
		for _, layout := range []string{"2 Jan 2006", "2 January 2006", "Jan 2 2006", "January 2 2006"} {
			if parsed, err := time.Parse(layout, value); err == nil {
				add(parsed.Format(time.DateOnly))
				break
			}
		}
	}
	return out
}

func bookingCode(text string) string {
	var found string
	for _, match := range booking.FindAllStringSubmatch(text, -1) {
		code := strings.ToUpper(match[1])
		if found != "" && found != code {
			return ""
		}
		found = code
	}
	return found
}

func addLegs(legs []Leg, seen map[Leg]bool, text, date string, utcDeparture time.Time, limit int, skip func(Leg) bool) []Leg {
	// A line containing a booking locator or URL should never trigger an airline search.
	if strings.Contains(text, "://") {
		return legs
	}
	text = booking.ReplaceAllString(text, " ")
	text = strings.NewReplacer("\u00a0", " ", "\u202f", " ").Replace(text)
	text = wordDate.ReplaceAllString(isoPattern.ReplaceAllString(text, " "), " ")
	for _, match := range flightPattern.FindAllStringSubmatch(text, -1) {
		if len(legs) >= limit {
			break
		}
		leg := Leg{Number: match[1] + match[2], Date: date, UTCDeparture: utcDeparture}
		if !seen[leg] && (skip == nil || !skip(leg)) {
			legs = append(legs, leg)
			seen[leg] = true
		}
	}
	return legs
}

func limitItinerary(result Itinerary, limit int) Itinerary {
	if len(result.Legs) > limit {
		result.Legs = result.Legs[:limit]
		result.Truncated = true
	}
	return result
}
