package itinerary

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEmailRecognizesExplicitDatedCancellation(t *testing.T) {
	const day = "2027-01-05"
	tests := []struct {
		name, mail string
		want       []Leg
		cancelled  bool
	}{
		{
			name: "forwarded subject and stale timed details",
			mail: "Subject: Fwd: Flight ua123 on 5 January 2027 has been canceled due to weather.\r\n\r\n" +
				"Booking reference: ABC12D\r\nUA123 | Departure: 5 January 2027 17:00",
			cancelled: true,
		},
		{
			name: "same flight number at a different time",
			mail: "Subject: Itinerary update\r\n\r\n" +
				"Flight UA123 on 5 January 2027 at 17:00 was cancelled. Replacement flight UA123 | Departure: 5 January 2027 19:00\r\n" +
				"UA123 2027-01-05\r\nUA123 | Departure: 5 January 2027 17:00",
			want: []Leg{{Number: "UA123", Date: day, LocalDeparture: "19:00"}},
		},
		{
			name: "wrapped statement and numeric-offset cancellation",
			mail: "Subject: Itinerary update\r\n\r\n" +
				"Flight UA123 on 2027-01-05T17:00:00-08:00\r\nwas cancelled.\r\n" +
				"UA123 | Departure: 5 January 2027 17:00\r\nReplacement flight UA123 | Departure: 5 January 2027 19:00",
			want: []Leg{{Number: "UA123", Date: day, LocalDeparture: "19:00"}},
		},
		{
			name: "cancelled UTC copy also rules out its coarse local date",
			mail: "Subject: Flight UA123 on 2027-01-06T01:00:00Z has been cancelled\r\n\r\n" +
				"UA123 2027-01-05\r\nUA123 | Departure: 2027-01-05T17:00:00-08:00",
			cancelled: true,
		},
		{
			name: "forwarded message attachment",
			mail: "Subject: Forwarded itinerary\r\nContent-Type: message/rfc822\r\n\r\n" +
				"Subject: Cancelled: Flight UA123 on 2027-01-05\r\n\r\nUA123 2027-01-05",
			cancelled: true,
		},
		{
			name: "cancellation evidence in HTML alternative",
			mail: "Subject: Itinerary update\r\nContent-Type: multipart/alternative; boundary=parts\r\n\r\n" +
				"--parts\r\nContent-Type: text/plain\r\n\r\nUA123 2027-01-05\r\n" +
				"--parts\r\nContent-Type: text/html\r\n\r\n<p>Flight UA123 on 5 January 2027 is cancelled.</p>" +
				"<p>Replacement flight UA456 2027-01-06</p>\r\n--parts--\r\n",
			want: []Leg{{Number: "UA456", Date: "2027-01-06"}},
		},
		{
			name: "negative wording preserves a confirmed flight",
			mail: "Subject: Your flight UA123 on 5 January 2027 is not cancelled\r\n\r\nUA123 2027-01-05",
			want: []Leg{{Number: "UA123", Date: day}},
		},
		{
			name: "notice date does not attach to unrelated undated text",
			mail: "Subject: Itinerary update\r\n\r\nFlight UA123 on 5 January 2027 was cancelled.\r\n" +
				"You can search for flights UA456 and UA789.",
			cancelled: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Email([]byte(tc.mail))
			if err != nil || !slices.Equal(result.Legs, tc.want) || result.CancellationOnly != tc.cancelled ||
				result.Truncated || result.BookingCode != "" {
				t.Fatalf("got %+v, %v, want flights %v and cancellation=%v", result, err, tc.want, tc.cancelled)
			}
		})
	}
}

func TestEmailCancellationPreservesDifferentCalendarDeparture(t *testing.T) {
	for _, tc := range []struct {
		name, start string
		instant     time.Time
	}{
		{"UTC", "DTSTART:20270105T210000Z", time.Date(2027, 1, 5, 21, 0, 0, 0, time.UTC)},
		{"floating", "DTSTART:20270105T190000", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := strings.Join([]string{
				"Subject: Flight UA123 on 5 January 2027 at 17:00 was cancelled",
				"Content-Type: multipart/mixed; boundary=parts", "",
				"--parts", "Content-Type: text/calendar", "", "BEGIN:VCALENDAR",
				"BEGIN:VEVENT", "UID:old", "DTSTART;TZID=America/Los_Angeles:20270105T170000", "SUMMARY:UA123", "END:VEVENT",
				"BEGIN:VEVENT", "UID:new", tc.start, "SUMMARY:UA123", "END:VEVENT",
				"END:VCALENDAR", "--parts--", "",
			}, "\r\n")
			result, err := Email([]byte(message))
			want := []Leg{{Number: "UA123", Date: "2027-01-05", UTCDeparture: tc.instant}}
			if err != nil || result.CancellationOnly || !slices.Equal(result.Legs, want) {
				t.Fatalf("calendar after cancellation returned %+v, %v, want %v", result, err, want)
			}
		})
	}
}

func TestEmailCancellationCopiesDoNotConsumeLegBudget(t *testing.T) {
	var message strings.Builder
	message.WriteString("Subject: Itinerary changes\r\n\r\n")
	for i := range MaxFlights {
		fmt.Fprintf(&message, "Flight UA%d on 2027-01-05 has been cancelled.\r\nUA%d 2027-01-05\r\n", 100+i, 100+i)
	}
	for i := range MaxFlights {
		fmt.Fprintf(&message, "UA%d 2027-01-06\r\n", 200+i)
	}
	result, err := Email([]byte(message.String()))
	if err != nil || result.Truncated || result.CancellationOnly || len(result.Legs) != MaxFlights {
		t.Fatalf("cancelled copies consumed budget: %+v, %v", result, err)
	}
	for i, leg := range result.Legs {
		if leg.Number != fmt.Sprintf("UA%d", 200+i) || leg.Date != "2027-01-06" {
			t.Fatalf("unexpected leg %+v", leg)
		}
	}
}
