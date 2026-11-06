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
		body    string
		extra   []string
		hours   []int
		locator string
	}{
		{name: "one precise leg", body: "UA123 2026-12-07", hours: []int{2}, locator: "ABC12D"},
		{name: "same local clock", body: "UA123 | Departure: 7 December 2026 18:00", hours: []int{2}, locator: "ABC12D"},
		{
			name: "distinct timed legs", body: "UA123 2026-12-07", hours: []int{2, 5},
			extra: []string{"BEGIN:VEVENT", "DTSTART;TZID=America/Los_Angeles:20261207T210000", "SUMMARY:UA123", "END:VEVENT"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := []string{
				"Subject: Your trip", "Content-Type: multipart/mixed; boundary=plano-test", "",
				"--plano-test", "Content-Type: text/plain", "", test.body,
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

func TestEmailKeepsDistinctLocalAndCalendarClocks(t *testing.T) {
	message := []string{
		"Subject: Your trip", "Content-Type: multipart/mixed; boundary=plano-test", "",
		"--plano-test", "Content-Type: text/plain", "", "UA123 | Departure: 7 December 2026 21:00",
		"--plano-test", "Content-Type: text/calendar", "", "BEGIN:VCALENDAR",
		"BEGIN:VEVENT", "DTSTART;TZID=America/Los_Angeles:20261207T180000", "SUMMARY:UA123", "END:VEVENT",
		"END:VCALENDAR", "--plano-test--", "",
	}
	result, err := Email([]byte(strings.Join(message, "\r\n")))
	want := []Leg{
		{Number: "UA123", Date: "2026-12-07", LocalDeparture: "21:00"},
		{Number: "UA123", Date: "2026-12-07", UTCDeparture: time.Date(2026, time.December, 8, 2, 0, 0, 0, time.UTC)},
	}
	if err != nil || !slices.Equal(result.Legs, want) {
		t.Fatalf("unexpected email itinerary: %+v, %v, want %v", result, err, want)
	}
}

func TestCalendarCancellationIsScopedToItsObject(t *testing.T) {
	cancelled := []string{
		"BEGIN:VCALENDAR", "BEGIN:VEVENT", "DTSTART:20261207T180000Z", "SUMMARY:UA123", "END:VEVENT",
		"meTHod:can", " cel", "END:VCALENDAR",
	}
	confirmed := []string{
		"BEGIN:VCALENDAR", "METHOD:REQUEST", "BEGIN:VEVENT", "DTSTART:20261207T210000Z", "SUMMARY:UA123",
		"BEGIN:VALARM", "METHOD:CANCEL", "END:VALARM", "END:VEVENT",
		"BEGIN:VEVENT", "DTSTART:20261207T180000Z", "SUMMARY:UA123", "END:VEVENT", "END:VCALENDAR",
	}
	for _, parser := range []struct {
		name  string
		parse func(string, string) (Itinerary, error)
	}{
		{name: "standalone", parse: Parse},
		{name: "subscription", parse: SubscriptionCalendar},
	} {
		t.Run(parser.name, func(t *testing.T) {
			result, err := parser.parse(strings.Join(cancelled, "\r\n"), "")
			if err != nil || len(result.Legs) != 0 {
				t.Fatalf("cancelled calendar returned %+v, %v", result, err)
			}
			mixed := append(slices.Clone(cancelled), confirmed...)
			result, err = parser.parse(strings.Join(mixed, "\r\n"), "")
			want := []Leg{{Number: "UA123", Date: "2026-12-07", UTCDeparture: time.Date(2026, time.December, 7, 21, 0, 0, 0, time.UTC)}}
			if err != nil || !slices.Equal(result.Legs, want) {
				t.Fatalf("mixed calendar returned %+v, %v, want %v", result, err, want)
			}
		})
	}
}

func TestEmailSuppressesTextLegRepeatedByCancellation(t *testing.T) {
	for _, test := range []struct {
		name, method, start, status, objectMethod, repeated string
	}{
		{
			name: "MIME method and local clock", method: "; method=CaNcEl", start: "DTSTART;TZID=America/Los_Angeles:20261207T180000",
			repeated: "UA123 | Departure: 7 December 2026 18:00",
		},
		{
			name: "calendar method and date", start: "DTSTART:20261207T180000Z", objectMethod: "method:can\r\n\tcel",
			repeated: "UA123 2026-12-07",
		},
		{
			name: "event status and matching UTC minute", start: "DTSTART:20261208T020030Z", status: "STATUS:CANCELLED",
			repeated: "UA123 | Departure: 2026-12-07T18:00:00-08:00",
		},
		{
			name: "recurrence identifier without start", method: "; method=CANCEL", start: "RECURRENCE-ID;TZID=America/Los_Angeles:20261207T180000",
			repeated: "UA123 | Departure: 7 December 2026 18:00",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := []string{
				"Subject: Itinerary update", "Content-Type: multipart/mixed; boundary=plano-test", "",
				"--plano-test", "Content-Type: text/plain", "", test.repeated,
				"UA123 | Departure: 7 December 2026 21:00", "UA456 2026-12-08",
				"--plano-test", "Content-Type: text/calendar" + test.method, "", "BEGIN:VCALENDAR",
				"BEGIN:VEVENT", test.start, "SUMMARY:UA123", test.status, "END:VEVENT", test.objectMethod, "END:VCALENDAR",
				"--plano-test", "Content-Type: text/calendar; method=REQUEST", "", "BEGIN:VCALENDAR", "METHOD:REQUEST",
				"BEGIN:VEVENT", "DTSTART;TZID=America/Los_Angeles:20261207T210000", "SUMMARY:UA123", "END:VEVENT",
				"END:VCALENDAR", "--plano-test--", "",
			}
			result, err := Email([]byte(strings.Join(message, "\r\n")))
			want := []Leg{
				{Number: "UA456", Date: "2026-12-08"},
				{Number: "UA123", Date: "2026-12-07", UTCDeparture: time.Date(2026, time.December, 8, 5, 0, 0, 0, time.UTC)},
			}
			if err != nil || result.CancellationOnly || !slices.Equal(result.Legs, want) {
				t.Fatalf("mixed email returned %+v, %v, want %v", result, err, want)
			}
		})
	}
}

func TestEmailCancellationOnly(t *testing.T) {
	for _, test := range []struct {
		name, body string
		event      []string
	}{
		{name: "dated flight", body: "UA123 2026-12-07", event: []string{"DTSTART:20261207T180000Z", "SUMMARY:UA123"}},
		{
			name: "UID only", body: "UA123 | Departure: 7 December 2026 18:00",
			event: []string{"UID:reservation@example.test", "DTSTAMP:20261106T080000Z", "ORGANIZER:mailto:airline@example.test", "SEQUENCE:2"},
		},
		{
			name: "local date differs from UTC", body: "UA123 | Departure: 7 December 2026 18:00",
			event: []string{"DTSTART:20261208T020000Z", "SUMMARY:UA123"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := []string{
				"Subject: Itinerary update", "Content-Type: multipart/alternative; boundary=plano-test", "",
				"--plano-test", "Content-Type: text/plain", "", test.body, "Booking reference: ABC12D",
				"--plano-test", "Content-Type: text/calendar; method=CANCEL", "", "BEGIN:VCALENDAR", "METHOD:CANCEL", "BEGIN:VEVENT",
			}
			message = append(message, test.event...)
			message = append(message, "END:VEVENT", "END:VCALENDAR", "--plano-test--", "")
			result, err := Email([]byte(strings.Join(message, "\r\n")))
			if err != nil || !result.CancellationOnly || len(result.Legs) != 0 || result.BookingCode != "" {
				t.Fatalf("cancellation email returned %+v, %v", result, err)
			}
		})
	}
}

func TestEmailCancellationMatchesStaleCalendarUID(t *testing.T) {
	cancelled := []string{
		"--plano-test", "Content-Type: text/calendar; method=CANCEL", "", "BEGIN:VCALENDAR", "METHOD:CANCEL",
		"BEGIN:VEVENT", "UID:reservation@example.test", "SEQUENCE:2", "END:VEVENT", "END:VCALENDAR",
	}
	stale := []string{
		"--plano-test", "Content-Type: text/calendar; method=REQUEST", "", "BEGIN:VCALENDAR", "METHOD:REQUEST",
		"BEGIN:VEVENT", "UID:reservation@example.test", "SEQUENCE:1", "DTSTART:20261207T180000Z", "SUMMARY:UA123", "END:VEVENT",
		"BEGIN:VEVENT", "UID:unrelated@example.test", "DTSTART:20261208T180000Z", "SUMMARY:UA456", "END:VEVENT", "END:VCALENDAR",
	}
	for _, test := range []struct {
		name          string
		first, second []string
	}{
		{name: "cancellation first", first: cancelled, second: stale},
		{name: "cancellation last", first: stale, second: cancelled},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := []string{
				"Subject: Itinerary update", "Content-Type: multipart/mixed; boundary=plano-test", "",
				"--plano-test", "Content-Type: text/plain", "", "UA123 2026-12-07",
			}
			message = append(message, test.first...)
			message = append(message, test.second...)
			message = append(message,
				"--plano-test", "Content-Type: text/calendar; method=REQUEST", "", "BEGIN:VCALENDAR", "METHOD:REQUEST",
				"BEGIN:VEVENT", "UID:reservation@example.test", "SEQUENCE:3", "DTSTART:20261207T210000Z", "SUMMARY:UA123", "END:VEVENT",
				"END:VCALENDAR", "--plano-test--", "")
			result, err := Email([]byte(strings.Join(message, "\r\n")))
			want := []Leg{
				{Number: "UA456", Date: "2026-12-08", UTCDeparture: time.Date(2026, time.December, 8, 18, 0, 0, 0, time.UTC)},
				{Number: "UA123", Date: "2026-12-07", UTCDeparture: time.Date(2026, time.December, 7, 21, 0, 0, 0, time.UTC)},
			}
			if err != nil || result.CancellationOnly || !slices.Equal(result.Legs, want) {
				t.Fatalf("updated email returned %+v, %v, want %v", result, err, want)
			}
		})
	}
}
