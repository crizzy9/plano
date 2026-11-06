package itinerary

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var passportCSVNow = time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC)

func TestPassportCSVPreservesOnlyFlightData(t *testing.T) {
	const uuid = "4256f3d8-2653-4977-962c-766c6d1e3041"
	input := "\ufeff\"Date\",Airline,Flight,From,To,Canceled,Diverted To,Gate Departure (Scheduled),Gate Departure (Actual),Gate Arrival (Scheduled),Gate Arrival (Actual),Aircraft Type Name,Tail Number,PNR,Seat,Notes,Flight Flighty ID\r\n" +
		"2026-11-01,ual,123,kewr,egll,false,,2026-11-01T18:00:00-05:00,2026-11-01T18:07:00-05:00,2026-11-02T05:55:00Z,2026-11-02T06:05:00Z,Boeing 787-9,n123ab,PNR123,SEAT123,\"private\nquoted \"\"note\"\"\"," + uuid + "\r\n"
	result, err := PassportCSV(strings.NewReader(input), passportCSVNow)
	if err != nil || len(result.Flights) != 1 || result.Skipped != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	flight := result.Flights[0]
	if !strings.HasPrefix(flight.ID, "archive-") || len(flight.ID) != len("archive-")+64 || flight.Number != "UAL123" ||
		flight.Origin != "KEWR" || flight.OriginICAO != "KEWR" || flight.Destination != "EGLL" || flight.DestinationICAO != "EGLL" ||
		flight.HistoryDate != "2026-11-01" || flight.Status != "arrived" || flight.Source != "flighty_csv" || !flight.ObservedAt.Equal(passportCSVNow) ||
		flight.TailNumber != "N123AB" || flight.AircraftModel != "Boeing 787-9" {
		t.Fatalf("unexpected archive flight = %+v", flight)
	}
	if flight.ActualArrival == nil || flight.ActualDeparture == nil || flight.ScheduledDeparture == nil || flight.ScheduledArrival == nil ||
		flight.ActualArrival.Sub(*flight.ActualDeparture) != 6*time.Hour+58*time.Minute ||
		flight.ActualArrival.Sub(*flight.ScheduledArrival) != 10*time.Minute || flight.ScheduledDeparture.Format(time.RFC3339) != "2026-11-01T23:00:00Z" {
		t.Fatalf("offset-qualified timings lost: %+v", flight)
	}
	raw, err := json.Marshal(flight)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{uuid, "PNR123", "SEAT123", "private"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("archive snapshot retained a private CSV value")
		}
	}
}

func TestPassportCSVLocalTimesPreserveDateAndArchiveIdentity(t *testing.T) {
	input := "From,To,Airline,Flight,Gate Departure (Scheduled),Gate Departure (Actual),Gate Arrival (Scheduled),Gate Arrival (Actual)\n" +
		"EWR,LHR,UAL,14,2026-01-02T08:50,2026-01-02T08:55,2026-01-02T21:05,2026-01-02T21:10\n"
	result, err := PassportCSV(strings.NewReader(input), passportCSVNow)
	if err != nil || len(result.Flights) != 1 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	flight := result.Flights[0]
	const priorID = "archive-3ee6330f465b7862c81dcc8899bf58c9da9de0bfff687287603444511957b1a8"
	if flight.HistoryDate != "2026-01-02" || flight.ID != priorID {
		t.Fatalf("archive date or identity changed: %+v", flight)
	}
	assertPassportCSVInstants(t, [4]*time.Time{flight.ScheduledDeparture, flight.ActualDeparture, flight.ScheduledArrival, flight.ActualArrival},
		[4]string{"2026-01-02T13:50:00Z", "2026-01-02T13:55:00Z", "2026-01-02T21:05:00Z", "2026-01-02T21:10:00Z"})
}

func TestPassportCSVLocalTimeResolution(t *testing.T) {
	const header = "Date,From,To,Gate Departure (Scheduled),Gate Departure (Actual),Gate Arrival (Scheduled),Gate Arrival (Actual)\n"
	for _, tt := range []struct {
		name string
		row  string
		want [4]string
	}{
		{
			name: "date line and ICAO codes",
			row:  "2025-01-02,NZAA,KLAX,2025-01-02T19:40,2025-01-02T19:50,2025-01-02T11:55,2025-01-02T12:10",
			want: [4]string{"2025-01-02T06:40:00Z", "2025-01-02T06:50:00Z", "2025-01-02T19:55:00Z", "2025-01-02T20:10:00Z"},
		},
		{
			name: "historical offset",
			row:  "1985-12-30,KTM,DEL,1985-12-30T11:00,,,",
			want: [4]string{"1985-12-30T05:30:00Z"},
		},
		{
			name: "spring gap at departure",
			row:  "2025-03-09,EWR,LHR,2025-03-09T01:59,2025-03-09T02:30,2025-03-09T14:30,",
			want: [4]string{"2025-03-09T06:59:00Z", "", "2025-03-09T14:30:00Z"},
		},
		{
			name: "fall fold at arrival",
			row:  "2025-10-25,EWR,LHR,2025-10-25T20:20,2025-10-25T20:30,2025-10-26T01:30,2025-10-26T06:50",
			want: [4]string{"2025-10-26T00:20:00Z", "2025-10-26T00:30:00Z", "", "2025-10-26T06:50:00Z"},
		},
		{
			name: "unknown zones preserve only the history date",
			row:  "2025-01-03,QZX,ZZZ,2025-01-03T08:00,2025-01-03T08:05,2025-01-03T17:30,2025-01-03T17:45",
		},
		{
			name: "scheduled local midnight is unknown and actual midnight is usable",
			row:  "2025-01-03,EWR,LHR,2025-01-03T00:00,2025-01-03T00:00,2025-01-04T00:00:00.000,2025-01-03T12:15",
			want: [4]string{"", "2025-01-03T05:00:00Z", "", "2025-01-03T12:15:00Z"},
		},
		{
			name: "explicit offset at midnight remains usable",
			row:  "2025-01-03,QZX,ZZZ,2025-01-03T00:00:00-05:00,,2025-01-04T00:00:00Z,",
			want: [4]string{"2025-01-03T05:00:00Z", "", "2025-01-04T00:00:00Z"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, err := PassportCSV(strings.NewReader(header+tt.row+"\n"), passportCSVNow)
			if err != nil || len(result.Flights) != 1 || result.Skipped != 0 {
				t.Fatalf("result = %+v, err = %v", result, err)
			}
			flight := result.Flights[0]
			if flight.HistoryDate != tt.row[:len(time.DateOnly)] {
				t.Fatalf("unexpected date: %s", flight.HistoryDate)
			}
			assertPassportCSVInstants(t, [4]*time.Time{flight.ScheduledDeparture, flight.ActualDeparture, flight.ScheduledArrival, flight.ActualArrival}, tt.want)
		})
	}
}

func assertPassportCSVInstants(t *testing.T, instants [4]*time.Time, want [4]string) {
	t.Helper()
	var got [4]string
	for i, instant := range instants {
		if instant != nil {
			got[i] = instant.Format(time.RFC3339)
		}
	}
	if got != want {
		t.Fatalf("gate times = %q, want %q", got, want)
	}
}

func TestPassportCSVSkipsInvalidRecordsAndDropsImpossibleTimings(t *testing.T) {
	input := "Date,From,To,Canceled,Diverted To,Gate Departure (Actual),Gate Arrival (Actual)\n" +
		"2026-11-06,EWR,LHR,false,,,\n" +
		"2026-11-07,EWR,LHR,false,,,\n" +
		"2026-11-01,EWR,LHR,true,,,\n" +
		"2026-11-01,EWR,LHR,false,MAN,,\n" +
		"2026-11-01,EWR,!,false,,,\n" +
		"1899-12-31,EWR,LHR,false,,,\n" +
		"bad,EWR,LHR,false,,2026-11-01T20:00:00Z,\n" +
		"2026-11-01,EWR,LHR,false\n" +
		"2026-11-01,EWR,LHR,false,,2026-11-01T20:00:00Z,2026-11-01T18:00:00Z\n" +
		"2026-11-05,EWR,LHR,false,,2026-11-05T20:00:00Z,2026-11-06T16:00:00Z\n"
	result, err := PassportCSV(strings.NewReader(input), passportCSVNow)
	if err != nil || len(result.Flights) != 2 || result.Skipped != 8 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	for _, flight := range result.Flights {
		if flight.ActualDeparture == nil || flight.ActualArrival != nil {
			t.Fatalf("unexpected actual timestamps: %+v", flight)
		}
	}
}

func TestPassportCSVStableIdentifiersDistinguishSameDayRepeats(t *testing.T) {
	input := "Date,From,To,Airline,Flight,Gate Departure (Scheduled),Flight Flighty ID\n" +
		"2025-01-02,EWR,SFO,UAL,123,2025-01-02T08:00,4256f3d8-2653-4977-962c-766c6d1e3041\n" +
		"2025-01-03,EWR,SFO,UAL,123,2025-01-03T08:00,4256F3D8-2653-4977-962C-766C6D1E3041\n" +
		"2025-01-02,EWR,SFO,UAL,123,2025-01-02T08:00,\n" +
		"2025-01-02,EWR,SFO,UAL,123,2025-01-02T08:00:00,\n" +
		"2025-01-02,EWR,SFO,UAL,123,2025-01-02T09:00,\n"
	result, err := PassportCSV(strings.NewReader(input), passportCSVNow)
	if err != nil || len(result.Flights) != 5 || result.Skipped != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	f := result.Flights
	if f[0].ID != f[1].ID || f[2].ID != f[3].ID || f[0].ID == f[2].ID || f[2].ID == f[4].ID {
		t.Fatal("stable identities did not distinguish same-day departures")
	}
}

func TestPassportCSVRejectsInvalidSchemaAndOversizedBatches(t *testing.T) {
	const header = "Date,From,To\n"
	const row = "2025-01-02,EWR,SFO\n"
	for _, input := range []string{
		"",
		"From,To\n",
		"Date,From,To, from \n",
		header + row + "2025-01-03,EWR,\"unterminated",
		header + strings.Repeat(row, maxPassportCSVRows+1),
	} {
		result, err := PassportCSV(strings.NewReader(input), passportCSVNow)
		if !errors.Is(err, ErrPassportCSV) || len(result.Flights) != 0 || result.Skipped != 0 {
			t.Fatalf("invalid export returned %d flights, err = %v", len(result.Flights), err)
		}
	}
	result, err := PassportCSV(strings.NewReader(header+strings.Repeat(row, maxPassportCSVRows)), passportCSVNow)
	if err != nil || len(result.Flights) != maxPassportCSVRows || result.Skipped != 0 {
		t.Fatalf("row limit returned %d flights, err = %v", len(result.Flights), err)
	}
}
