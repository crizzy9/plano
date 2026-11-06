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
	want := []Leg{{Number: "UA123", Date: "2026-12-07"}, {Number: "UA456", Date: "2026-12-09", UTCDeparture: time.Date(2026, 12, 9, 2, 30, 0, 0, time.UTC)}}
	if !reflect.DeepEqual(it.Legs, want) {
		t.Fatalf("legs %#v", it.Legs)
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
