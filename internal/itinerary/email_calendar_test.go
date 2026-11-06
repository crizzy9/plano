package itinerary

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEmailPrefersTimedCalendarLegsToDuplicateBodyDate(t *testing.T) {
	for _, test := range []struct {
		name    string
		extra   []string
		hours   []int
		locator string
	}{
		{name: "one precise leg", hours: []int{2}, locator: "ABC12D"},
		{
			name: "distinct timed legs", hours: []int{2, 5},
			extra: []string{"BEGIN:VEVENT", "DTSTART;TZID=America/Los_Angeles:20261207T210000", "SUMMARY:UA123", "END:VEVENT"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := []string{
				"Subject: Your trip", "Content-Type: multipart/mixed; boundary=plano-test", "",
				"--plano-test", "Content-Type: text/plain", "", "UA123 2026-12-07",
				"--plano-test", "Content-Type: text/calendar", "", "BEGIN:VCALENDAR",
				"BEGIN:VEVENT", "DTSTART;TZID=America/Los_Angeles:20261207T180000", "SUMMARY:UA123",
				"DESCRIPTION:Booking reference: ABC12D", "END:VEVENT",
			}
			message = append(message, test.extra...)
			message = append(message, "END:VCALENDAR", "--plano-test--", "")
			result, err := Email([]byte(strings.Join(message, "\r\n")))
			if err != nil {
				t.Fatal(err)
			}
			var want []Leg
			for _, hour := range test.hours {
				want = append(want, Leg{Number: "UA123", Date: "2026-12-07", UTCDeparture: time.Date(2026, time.December, 8, hour, 0, 0, 0, time.UTC)})
			}
			if !slices.Equal(result.Legs, want) || result.BookingCode != test.locator {
				t.Fatalf("unexpected email itinerary: %+v, want legs %v and locator %q", result, want, test.locator)
			}
		})
	}
}
