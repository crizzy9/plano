package itinerary

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionCalendarFiltersHistoryBeforeApplyingLegLimit(t *testing.T) {
	var feed strings.Builder
	feed.WriteString("BEGIN:VCALENDAR\r\n")
	for index := 1; index <= 1024; index++ {
		fmt.Fprintf(&feed, "BEGIN:VEVENT\r\nDTSTART:20260101T120000Z\r\nSUMMARY:Flight UA%d\r\nEND:VEVENT\r\n", index)
	}
	feed.WriteString("BEGIN:VEVENT\r\nDTSTART:20261201T120000Z\r\nSUMMARY:Flight UA123\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	upcoming, err := SubscriptionCalendar(feed.String(), "2026-11-30")
	if err != nil || upcoming.Truncated || len(upcoming.Legs) != 1 || upcoming.Legs[0].Date != "2026-12-01" {
		t.Fatalf("unexpected upcoming result: %+v, %v", upcoming, err)
	}
	history, err := SubscriptionCalendar(feed.String(), "")
	if err != nil || !history.Truncated || len(history.Legs) != 1024 {
		t.Fatalf("unexpected history result: %d legs, truncated=%v, %v", len(history.Legs), history.Truncated, err)
	}
}

func TestEmailItineraryWithHTMLAndMultipleLegs(t *testing.T) {
	html := `<html><head><script>UA999 on 5 Jan 2027</script></head><body>
<table><tr><td>UA&nbsp;123</td><td>7 December 2026</td></tr>
<tr><td>UA456</td><td>December 9, 2026</td></tr></table><p>Booking reference: ABC12D</p></body></html>`
	raw := "Subject: Your trip\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(html))
	it, err := Email([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := []Leg{{Number: "UA123", Date: "2026-12-07"}, {Number: "UA456", Date: "2026-12-09"}}
	if !reflect.DeepEqual(it.Legs, want) || it.BookingCode != "ABC12D" {
		t.Fatalf("itinerary %#v", it)
	}
}

func TestCalendarUsesEachEventDateAndSkipsCancellation(t *testing.T) {
	ics := strings.Join([]string{"BEGIN:VCALENDAR", "BEGIN:VEVENT", "DTSTART;TZID=America/Los_Angeles:20261207T180000", "SUMMARY:Flight UA", " 123", "END:VEVENT",
		"BEGIN:VEVENT", "DTSTART:20261209T023000Z", "SUMMARY:Flight UA456", "END:VEVENT",
		"BEGIN:VEVENT", "DTSTART:20261210T180000Z", "SUMMARY:Flight UA789", "STATUS:CANCELLED", "END:VEVENT", "END:VCALENDAR"}, "\r\n")
	it, err := Parse(ics, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []Leg{
		{Number: "UA123", Date: "2026-12-07", UTCDeparture: time.Date(2026, 12, 8, 2, 0, 0, 0, time.UTC)},
		{Number: "UA456", Date: "2026-12-09", UTCDeparture: time.Date(2026, 12, 9, 2, 30, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(it.Legs, want) {
		t.Fatalf("legs %#v", it.Legs)
	}
}

func TestCalendarDepartureWithTZID(t *testing.T) {
	tests := []struct {
		name, start, date, departure string
	}{
		{"quoted parameters", `DTSTART;X-NOTE="stop:;gate";tzid="Asia/Tokyo":20261207T030000`, "2026-12-07", "2026-12-06T18:00:00Z"},
		{"fall back west", "DTSTART;TZID=America/New_York:20071104T013000", "2007-11-04", "2007-11-04T05:30:00Z"},
		{"spring forward west", "DTSTART;TZID=America/New_York:20070311T023000", "2007-03-11", "2007-03-11T07:30:00Z"},
		{"fall back east", "DTSTART;TZID=Australia/Sydney:20260405T023000", "2026-04-05", "2026-04-04T15:30:00Z"},
		{"spring forward east", "DTSTART;TZID=Australia/Sydney:20261004T023000", "2026-10-04", "2026-10-03T16:30:00Z"},
		{"skipped day", "DTSTART;TZID=Pacific/Apia:20111230T120000", "2011-12-30", "2011-12-30T22:00:00Z"},
		{"all day", "DTSTART;VALUE=DATE:20261207", "2026-12-07", ""},
		{"floating", "DTSTART:20261207T180000", "2026-12-07", ""},
		{"unrecognized zone", `DTSTART;TZID="Pacific Standard Time":20261207T180000`, "2026-12-07", ""},
		{"host local zone", "DTSTART;TZID=Local:20261207T180000", "2026-12-07", ""},
		{"host local file", "DTSTART;TZID=localtime:20261207T180000", "2026-12-07", ""},
		{"path traversal", "DTSTART;TZID=../etc/localtime:20261207T180000", "2026-12-07", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ics := "BEGIN:VCALENDAR\nBEGIN:VEVENT\n" + test.start + "\nSUMMARY:UA123\nEND:VEVENT\nEND:VCALENDAR\n"
			want := Leg{Number: "UA123", Date: test.date}
			if test.departure != "" {
				var err error
				want.UTCDeparture, err = time.Parse(time.RFC3339, test.departure)
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := Parse(ics, "")
			if err != nil || !reflect.DeepEqual(got.Legs, []Leg{want}) {
				t.Fatalf("legs = %#v, error = %v, want %#v", got.Legs, err, want)
			}
		})
	}
}

func TestCalendarBookingComesFromFlightEvent(t *testing.T) {
	other := "BEGIN:VEVENT\nDTSTART:20261207T180000Z\nSUMMARY:Hotel check-in\nDESCRIPTION:Booking reference: ABC12D\nEND:VEVENT\n"
	flight := "BEGIN:VEVENT\nDTSTART:20261207T200000Z\nSUMMARY:Flight UA123\n"
	for _, test := range []struct{ name, events, code string }{
		{"unrelated booking ignored", other + flight + "END:VEVENT\n", ""},
		{"flight booking preserved", other + flight + "DESCRIPTION:Booking reference: XYZ12B\nEND:VEVENT\n", "XYZ12B"},
		{"duplicate flight booking disagreement", flight + "DESCRIPTION:Booking reference: ABC12D\nEND:VEVENT\n" +
			flight + "DESCRIPTION:Booking reference: XYZ12B\nEND:VEVENT\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := Parse("BEGIN:VCALENDAR\n"+test.events+"END:VCALENDAR\n", "")
			if err != nil || len(got.Legs) != 1 || got.Legs[0].Number != "UA123" || got.BookingCode != test.code {
				t.Fatalf("itinerary = %#v, error = %v, want UA123 with booking %q", got, err, test.code)
			}
		})
	}
}

func TestCalendarIgnoresAlarmProperties(t *testing.T) {
	alarm := "BEGIN:VALARM\nTRIGGER:-PT30M\nACTION:EMAIL\nATTENDEE:mailto:traveler@example.test\n" +
		"SUMMARY:Flight UA999\nDESCRIPTION:Booking reference: XYZ12B\nEND:VALARM\n"
	ics := "BEGIN:VCALENDAR\nBEGIN:VEVENT\nDTSTART:20261207T180000Z\nSUMMARY:Flight UA123\n" +
		"DESCRIPTION:Booking reference: ABC12D\n" + alarm + "END:VEVENT\n" +
		"BEGIN:VEVENT\nDTSTART:20261208T180000Z\nSUMMARY:Meeting\n" + alarm + "END:VEVENT\nEND:VCALENDAR\n"
	got, err := Parse(ics, "")
	want := []Leg{{Number: "UA123", Date: "2026-12-07", UTCDeparture: time.Date(2026, 12, 7, 18, 0, 0, 0, time.UTC)}}
	if err != nil || !reflect.DeepEqual(got.Legs, want) || got.BookingCode != "ABC12D" {
		t.Fatalf("itinerary = %#v, error = %v, want the flight and booking from the event", got, err)
	}
}

func TestPlainImportsDigitFirstCarrier(t *testing.T) {
	got, err := Parse("Flight 6E 123A", "2026-12-07")
	want := []Leg{{Number: "6E123A", Date: "2026-12-07"}}
	if err != nil || !reflect.DeepEqual(got.Legs, want) {
		t.Fatalf("legs = %#v, error = %v, want %#v", got.Legs, err, want)
	}
}

func TestPlainIgnoresLocatorAsFlight(t *testing.T) {
	it, err := Parse("UA123\nBooking reference: AB123C\n", "2027-02-03")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(it.Legs, []Leg{{Number: "UA123", Date: "2027-02-03"}}) || it.BookingCode != "AB123C" {
		t.Fatalf("itinerary %#v", it)
	}
}

func TestMultilineBookingLocatorIsNeverAFlightCandidate(t *testing.T) {
	text := "2026-11-05\nPNR:\nUA1234\nFlight AA123\n"
	it, err := Parse(text, "")
	if err != nil || !reflect.DeepEqual(it.Legs, []Leg{{Number: "AA123", Date: "2026-11-05"}}) || it.BookingCode != "UA1234" {
		t.Fatalf("itinerary %#v: %v", it, err)
	}
}

func TestManualImportReportsLegLimit(t *testing.T) {
	it, err := Parse("UA123 UA124 UA125 UA126 UA127 UA128 UA129 UA130 UA131", "2026-11-05")
	if err != nil || len(it.Legs) != MaxFlights || !it.Truncated {
		t.Fatalf("itinerary %#v: %v", it, err)
	}
}
