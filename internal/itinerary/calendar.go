package itinerary

import (
	"strings"
	"time"
)

// SubscriptionCalendar reads a bounded feed so callers can select a page of
// upcoming flights before falling back to history.
func SubscriptionCalendar(text string) (Itinerary, error) { return calendar(text, 1024) }

func calendar(text string, limit int) (Itinerary, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\n ", ""), "\n\t", "")
	result := Itinerary{}
	seen := make(map[Leg]bool)
	var active, cancelled, recurring bool
	var start, summary, description string
	var utcDeparture time.Time
	var locator string
	var conflicting bool
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, _, _ = strings.Cut(key, ";")
		switch strings.ToUpper(key) {
		case "BEGIN":
			if strings.EqualFold(value, "VEVENT") {
				active, cancelled, recurring, start, summary, description = true, false, false, "", "", ""
				utcDeparture = time.Time{}
			}
		case "END":
			if !strings.EqualFold(value, "VEVENT") || !active {
				continue
			}
			active = false
			if cancelled || recurring || start == "" {
				continue
			}
			before := len(result.Legs)
			result.Legs = addLegs(result.Legs, seen, summary, start, utcDeparture, limit+1)
			if before == len(result.Legs) {
				result.Legs = addLegs(result.Legs, seen, description, start, utcDeparture, limit+1)
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
					utcDeparture, _ = time.Parse("20060102T150405Z", value)
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

func unescapeCalendar(value string) string {
	return strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`).Replace(value)
}
