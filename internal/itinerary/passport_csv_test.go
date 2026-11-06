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

func TestPassportCSVOffsetFreeTimesSupplyDateButNotInstants(t *testing.T) {
	input := "From,To,Airline,Flight,Gate Departure (Scheduled),Gate Departure (Actual),Gate Arrival (Scheduled),Gate Arrival (Actual)\n" +
		"EWR,LHR,UAL,14,2026-01-02T08:50,2026-01-02T08:55,2026-01-02T21:05,2026-01-02T21:10\n"
	result, err := PassportCSV(strings.NewReader(input), passportCSVNow)
	if err != nil || len(result.Flights) != 1 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	flight := result.Flights[0]
	if flight.HistoryDate != "2026-01-02" || flight.ScheduledDeparture != nil || flight.ActualDeparture != nil ||
		flight.ScheduledArrival != nil || flight.ActualArrival != nil {
		t.Fatalf("unexpected absolute times or date: %+v", flight)
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
