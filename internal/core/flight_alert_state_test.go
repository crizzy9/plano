package core

import (
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestFlightAlertRelevanceFollowsPhaseAndAvailableFields(t *testing.T) {
	now := time.Now().UTC()
	flight := feeds.Flight{
		Status: "boarding", ScheduledDeparture: &now, EstimatedDeparture: &now, EstimatedArrival: &now,
		DepartureGate: "A4", ArrivalGate: "B2", Baggage: "3", TailNumber: "N123AA",
	}
	for _, test := range []struct {
		name   string
		change func(*feeds.Flight)
		keep   []string
		drop   []string
	}{
		{
			name: "boarding", change: func(*feeds.Flight) {},
			keep: []string{"boarding", "check_in", "departure_gate", "etd", "delay_prediction", "eta"},
			drop: []string{"cancelled", "diverted"},
		},
		{
			name: "later actual time without earlier milestones", change: func(f *feeds.Flight) { f.ActualLanding = &now },
			keep: []string{"eta", "landed", "arrival_gate", "baggage"},
			drop: []string{"boarding", "check_in", "departure_gate", "etd", "delay_prediction"},
		},
		{
			name: "at gate", change: func(f *feeds.Flight) { f.Status, f.ActualArrival = "arrived", &now },
			keep: []string{"arrival_gate", "baggage", "tail", "taxi_out", "airborne", "landed", "arrived"},
			drop: []string{"boarding", "eta"},
		},
		{
			name: "cancelled", change: func(f *feeds.Flight) { f.Status = "cancelled" },
			keep: []string{"cancelled", "ground_stop", "connection_risk"},
			drop: []string{"boarding", "check_in", "departure_gate", "eta", "arrival_gate", "baggage", "tail", "diverted"},
		},
		{
			name: "diverted", change: func(f *feeds.Flight) { f.Status = "diverted" },
			keep: []string{"diverted", "airborne", "tail"},
			drop: []string{"arrival_gate", "baggage", "eta"},
		},
		{
			name: "withdrawn fields", change: func(f *feeds.Flight) {
				f.EstimatedArrival, f.EstimatedDeparture = nil, nil
				f.DepartureGate, f.ArrivalGate, f.Baggage, f.TailNumber = "", "", " ", ""
			},
			keep: []string{"check_in"},
			drop: []string{"departure_gate", "arrival_gate", "baggage", "tail", "etd", "eta"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			updated := flight
			test.change(&updated)
			for _, kind := range test.keep {
				if !flightAlertRelevant(kind, updated) {
					t.Errorf("%s should remain relevant", kind)
				}
			}
			for _, kind := range test.drop {
				if flightAlertRelevant(kind, updated) {
					t.Errorf("%s should become irrelevant", kind)
				}
			}
		})
	}
}

func TestSupersededFlightAlertKindsKeepMilestonesAndMinorEstimateChanges(t *testing.T) {
	now := time.Now().UTC()
	before := feeds.Flight{
		Status: "boarding", Origin: "SFO", OriginICAO: "KSFO", Destination: "JFK", DestinationICAO: "KJFK",
		ScheduledDeparture: &now, EstimatedDeparture: &now, EstimatedArrival: &now,
		DepartureGate: "A4", ArrivalGate: "B2", Baggage: "3", TailNumber: "N123AA",
	}
	for _, test := range []struct {
		name   string
		change func(*feeds.Flight)
		want   []string
		extra  []Event
	}{
		{
			name: "subthreshold estimate movement", change: func(f *feeds.Flight) { f.EstimatedDeparture = new(now.Add(time.Minute)) },
		},
		{
			name: "material estimate successor", change: func(f *feeds.Flight) { f.EstimatedDeparture = new(now.Add(6 * time.Minute)) },
			want: []string{"etd"},
		},
		{
			name: "same airport with alias only", change: func(f *feeds.Flight) { f.Origin, f.OriginICAO = "KSFO", "" },
		},
		{
			name: "new destination with same gate and carousel values", change: func(f *feeds.Flight) { f.Destination, f.DestinationICAO = "EWR", "KEWR" },
			want: []string{"arrival_gate", "eta", "baggage"},
		},
		{
			name: "withdrawn departure gate", change: func(f *feeds.Flight) { f.DepartureGate = "" },
			want: []string{"departure_gate"},
		},
		{
			name: "aircraft pushed back", change: func(f *feeds.Flight) { f.Status, f.ActualDeparture = "taxi_out", &now },
			want: []string{"boarding", "check_in", "departure_gate", "etd", "delay_prediction"},
		},
		{
			name: "duplicate successor kinds", change: func(*feeds.Flight) {},
			want: []string{"baggage"}, extra: []Event{{Kind: "baggage"}, {Kind: "baggage"}, {Kind: "airborne"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			after := before
			test.change(&after)
			events := append(changes(before, after), test.extra...)
			if got := supersededFlightAlertKinds(before, after, events); !slices.Equal(got, test.want) {
				t.Errorf("superseded kinds = %v, want %v", got, test.want)
			}
		})
	}
}
