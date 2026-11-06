package itinerary

import (
	"slices"
	"testing"
	"time"
)

func TestParseLabeledDeparture(t *testing.T) {
	tests := []struct {
		name, input, date string
		want              []Leg
	}{
		{
			name:  "overnight inline",
			input: "Flight UA123 | Departure: 7 December 2026 22:00 | Arrival: 8 December 2026 06:00",
			want:  []Leg{{Number: "UA123", Date: "2026-12-07", LocalDeparture: "22:00"}},
		},
		{
			name:  "consecutive multiline flights",
			input: "Flight UA123\nDeparture: 7 December 2026 at 10:05 pm\nArrival: 8 December 2026 06:00\nFlight DL456\nDeparture date: 8 December 2026\nDeparture time: 12:15 AM\nArrival: 9 December 2026",
			want: []Leg{
				{Number: "UA123", Date: "2026-12-07", LocalDeparture: "22:05"},
				{Number: "DL456", Date: "2026-12-08", LocalDeparture: "00:15"},
			},
		},
		{
			name:  "exact offset crosses UTC midnight",
			input: "UA123 | Departure: 2026-12-07T22:00:00-05:00 | Arrival: 2026-12-08T06:00:00-05:00",
			want:  []Leg{{Number: "UA123", Date: "2026-12-07", UTCDeparture: time.Date(2026, 12, 8, 3, 0, 0, 0, time.UTC)}},
		},
		{
			name:  "explicit UTC clock before date",
			input: "UA123 | Departure: 9:00 AM UTC, 7 December 2026",
			want:  []Leg{{Number: "UA123", Date: "2026-12-07", UTCDeparture: time.Date(2026, 12, 7, 9, 0, 0, 0, time.UTC)}},
		},
		{
			name:  "manual date with departure time",
			input: "UA123\nDeparture time: 7:00",
			date:  "2026-12-07",
			want:  []Leg{{Number: "UA123", Date: "2026-12-07", LocalDeparture: "07:00"}},
		},
		{
			name:  "explicit prefix date beside arrival",
			input: "UA123 2026-12-07\nArrival: 2026-12-08",
			want:  []Leg{{Number: "UA123", Date: "2026-12-07"}},
		},
		{
			name:  "explicit preceding date heading beside arrival",
			input: "Departure: 7 December 2026\nFlight UA123\nArrival: 8 December 2026",
			want:  []Leg{{Number: "UA123", Date: "2026-12-07"}},
		},
		{
			name:  "explicit preceding date heading with clock",
			input: "Departure: 7 December 2026\nFlight UA123\nDeparture time: 7:00 AM\nArrival: 8 December 2026",
			want:  []Leg{{Number: "UA123", Date: "2026-12-07", LocalDeparture: "07:00"}},
		},
		{
			name:  "arrival heading cannot date another flight",
			input: "7 December 2026\nUA123\nArrival: 8 December 2026\nDL456",
			want:  []Leg{{Number: "UA123", Date: "2026-12-07"}},
		},
		{
			name:  "structured departure prevents unrelated sole date fallback",
			input: "Flight UA123\nTicket issued 8 December 2026\nUA123 | Departure: 7 December 2026 22:00 | Arrival: 8 December 2026 06:00",
			want:  []Leg{{Number: "UA123", Date: "2026-12-07", LocalDeparture: "22:00"}},
		},
		{
			name:  "simple date heading",
			input: "7 December 2026\nUA123\n8 December 2026\nDL456",
			want: []Leg{
				{Number: "UA123", Date: "2026-12-07"},
				{Number: "DL456", Date: "2026-12-08"},
			},
		},
		{
			name:  "distinct clocks replace date-only duplicate",
			input: "UA123 2026-12-07\nUA123 | Departure: 2026-12-07 7:00\nUA123 | Departure: 2026-12-07 17:00",
			want: []Leg{
				{Number: "UA123", Date: "2026-12-07", LocalDeparture: "07:00"},
				{Number: "UA123", Date: "2026-12-07", LocalDeparture: "17:00"},
			},
		},
		{
			name:  "new departure heading after completed schedule",
			input: "UA123 | Departure: 2026-12-07 22:00 | Arrival: 2026-12-08\nDeparture: 2026-12-09\nDL456",
			want: []Leg{
				{Number: "UA123", Date: "2026-12-07", LocalDeparture: "22:00"},
				{Number: "DL456", Date: "2026-12-09"},
			},
		},
		{
			name:  "arrival without departure",
			input: "Flight UA123\nArrival: 8 December 2026\nDL456",
		},
		{
			name:  "unknown labels with two dates",
			input: "Flight UA123 | Outbound: 7 December 2026 | Inbound: 8 December 2026",
		},
		{
			name:  "unlabeled dates are ambiguous",
			input: "UA123 2026-12-07 22:00 2026-12-08 06:00",
		},
		{
			name:  "unknown departure time zone",
			input: "UA123 | Departure: 7 December 2026 17:00 PST",
		},
		{
			name:  "malformed departure clock",
			input: "UA123 | Departure: 7 December 2026 25:10",
		},
		{
			name:  "invalid zero hour for twelve hour clock",
			input: "UA123 | Departure: 7 December 2026 0:00 PM",
		},
		{
			name:  "malformed departure offset",
			input: "UA123 | Departure: 2026-12-07T22:00:00+25:00",
			date:  "2026-12-07",
		},
		{
			name:  "partial RFC3339 offset cannot be accepted",
			input: "UA123 | Departure: 2026-12-07T22:00:00+05:00:30",
		},
		{
			name:  "malformed explicit time field",
			input: "UA123\nDeparture date: 7 December 2026\nDeparture time: TBD",
		},
		{
			name:  "conflicting departure fields",
			input: "UA123\nDeparture: 7 December 2026\nDeparture: 8 December 2026",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Parse(test.input, test.date)
			if err != nil || !slices.Equal(got.Legs, test.want) {
				t.Fatalf("Parse() = %#v, %v; want legs %#v", got, err, test.want)
			}
		})
	}
}
