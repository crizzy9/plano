package core

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

func TestPredictionFeaturesUseMatchingAndKnownSignals(t *testing.T) {
	now := time.Date(2026, time.November, 5, 17, 30, 0, 0, time.UTC)
	departure := now.Add(30 * time.Minute)
	arrival := departure.Add(4 * time.Hour)
	published := departure.Add(10 * time.Minute)
	flight := feeds.Flight{ID: "main", Number: "UA7", Status: "scheduled", Origin: "SFO", Destination: "JFK", OriginICAO: "KSFO", DestinationICAO: "KJFK",
		ScheduledDeparture: &departure, ScheduledArrival: &arrival, EstimatedDeparture: &published}
	inbound := []feeds.Flight{{ID: "inbound", Number: "UA8", Status: "airborne", DestinationICAO: "KSFO", Destination: "KSFO",
		ObservedAt: now, ScheduledArrival: new(departure.Add(-20 * time.Minute)), EstimatedArrival: new(departure.Add(15 * time.Minute))}}
	stopEnd := departure.Add(70 * time.Minute)
	advisories := []feeds.Advisory{
		{Airport: "KJFK", Direction: "arrival", Kind: "ground_stop", Reason: "weather and ATC staffing", Source: "faa_nas", ObservedAt: now, EndsAt: &stopEnd},
		{Airport: "KSFO", Direction: "departure", Kind: "departure_delay", Reason: "airport volume", Source: "faa_nas", ObservedAt: now, DelayMinutes: new(40)},
		{Airport: "KSFO", Direction: "arrival", Kind: "arrival_delay", Reason: "unrelated direction", Source: "faa_nas", ObservedAt: now, DelayMinutes: new(100)},
		{Airport: "JFK", Direction: "arrival", Kind: "airport_restriction", Reason: "general aviation only", Source: "faa_nas", ObservedAt: now, DelayMinutes: new(90)},
		{Airport: "JFK", Direction: "arrival", Kind: "arrival_delay", Reason: "future report", Source: "faa_nas", ObservedAt: now.Add(time.Minute), DelayMinutes: new(110)},
	}
	features, ok := ExtractDelayFeatures(flight, inbound, advisories, now)
	want := predict.Features{AirlineDelayMinutes: 10, InboundDelayMinutes: 55, AirportDelayMinutes: 40, GroundStopMinutes: 70, Weather: true, AirTrafficControl: true}
	if !ok || features != want {
		t.Fatalf("features = %+v, %t, want %+v", features, ok, want)
	}
	forecast := Predict(flight, inbound, advisories, now)
	if forecast == nil || forecast.Method != "operational_estimate" || forecast.DelayMinutes != 70 || !forecast.Departure.Equal(stopEnd) || len(forecast.Reasons) != 3 {
		t.Fatalf("operational forecast = %+v", forecast)
	}
	if !strings.Contains(forecast.Reasons[0], "UA8") || !strings.Contains(forecast.Reasons[1], "FAA") || !flight.EstimatedDeparture.Equal(published) {
		t.Fatalf("published estimate or reason changed: %v, %+v", flight.EstimatedDeparture, forecast.Reasons)
	}
}

func TestPredictionUsesTrainedModelAndRetainsOperationalFallback(t *testing.T) {
	start := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]predict.Example, 40)
	for i := range rows {
		observed := start.Add(time.Duration(i) * 2 * time.Hour)
		departure := observed.Add(time.Hour)
		airlineDelay := 5 * (1 + i%5)
		rows[i] = predict.Example{
			FlightID: fmt.Sprintf("synthetic-%d", i), ObservedAt: observed, ScheduledDeparture: departure,
			DepartedAt: departure.Add(time.Duration(10+2*airlineDelay) * time.Minute),
			Features:   predict.Features{AirlineDelayMinutes: float64(airlineDelay)},
		}
	}
	model, _, err := predict.Fit(rows, predict.Options{HoldoutFraction: 0.2, Ridge: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	row := rows[39]
	published := row.ScheduledDeparture.Add(25 * time.Minute)
	flight := feeds.Flight{ID: row.FlightID, Number: "UA7", Status: "scheduled", Origin: "SFO", Destination: "JFK",
		ScheduledDeparture: &row.ScheduledDeparture, EstimatedDeparture: &published}
	got := PredictWithModel(model, flight, nil, nil, row.ObservedAt)
	if got == nil || got.Method != "learned_estimate" || got.DelayMinutes != 60 || got.ModelVersion != model.ID() || got.BaselineMinutes == nil || len(got.FeatureContributions) < 7 || len(got.Reasons) == 0 {
		t.Fatalf("learned forecast = %+v", got)
	}
	if !flight.EstimatedDeparture.Equal(published) || !flight.ScheduledDeparture.Equal(row.ScheduledDeparture) {
		t.Fatal("forecast modified published flight times")
	}
	advisory := feeds.Advisory{Airport: "SFO", Direction: "departure", Kind: "departure_delay", Reason: "weather", Source: "faa_nas", DelayMinutes: new(35), ObservedAt: row.ObservedAt}
	got = PredictWithModel(model, flight, nil, []feeds.Advisory{advisory}, row.ObservedAt)
	if got == nil || got.Method != "operational_estimate" || got.DelayMinutes != 35 || got.ModelVersion != "" {
		t.Fatalf("fallback forecast = %+v", got)
	}
	flight.Status = "airborne"
	if got := PredictWithModel(model, flight, nil, nil, row.ObservedAt); got != nil {
		t.Fatalf("predicted already departed flight: %+v", got)
	}
}
