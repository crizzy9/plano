package core

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

func TestStoreUsesSameAvailableTerminalWeatherForPredictionAndSamples(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	model := fitTerminalWeatherFixture(t, now)
	cache := NewWeatherForecastCache(nil)
	departure := now.Add(3 * time.Minute)
	period := feeds.TAFPeriod{Airport: "KSFO", Conditions: "low visibility", Qualifier: "prevailing",
		IssuedAt: now.Add(-time.Hour), StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour)}
	cache.byStation["KSFO"] = stationWeatherForecast{availableAt: now.Add(-time.Minute), periods: []feeds.TAFPeriod{period}}
	db := filepath.Join(t.TempDir(), "weather-delay.db")
	store, err := OpenWithOptions(db, StoreOptions{DelayModel: model, WeatherForecast: cache, CollectDelaySamples: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	flight := feeds.Flight{ID: "UAL89-terminal-weather", Number: "UA89", Status: "scheduled", Source: "flightaware_aeroapi",
		Origin: "SFO", OriginICAO: "KSFO", Destination: "JFK", DestinationICAO: "KJFK",
		ScheduledDeparture: &departure, ObservedAt: now}
	earlier := flight
	earlier.ID, earlier.ObservedAt = "UAL89-before-weather", now.Add(-2*time.Minute)
	prior := flight
	prior.ObservedAt = earlier.ObservedAt
	if err := store.Observe(t.Context(), prior, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []feeds.Flight{earlier, flight} {
		if err := store.observeTracked(t.Context(), snapshot, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	learned, err := store.Details(t.Context(), flight.ID)
	if err != nil || learned.Prediction == nil {
		t.Fatalf("expected weather-only prediction: %+v (%v)", learned.Prediction, err)
	}
	forecast := learned.Prediction
	if forecast.ModelVersion != model.ID() || forecast.ValidUntil == nil || !forecast.ValidUntil.Equal(period.EndsAt) ||
		forecast.TerminalWeatherEvidence != nil ||
		!slices.ContainsFunc(forecast.Reasons, func(reason string) bool { return strings.Contains(reason, "NOAA forecasts low visibility at KSFO") }) ||
		!slices.ContainsFunc(forecast.FeatureContributions, func(c predict.Contribution) bool {
			return c.Feature == "terminal_weather_forecast" && c.Minutes > 1
		}) {
		t.Fatalf("weather prediction did not explain or bound its signal: %+v", forecast)
	}
	if details, err := store.Details(t.Context(), earlier.ID); err != nil || details.Prediction != nil {
		t.Fatalf("earlier flight observation used a late forecast: %+v (%v)", details.Prediction, err)
	}
	delayAlerts := func(details Details) int {
		count := 0
		for _, event := range details.Events {
			if event.Kind == "delay_prediction" {
				count++
			}
		}
		return count
	}
	if delayAlerts(learned) != 1 {
		t.Fatalf("weather estimate did not create its initial delay event: %+v", learned.Events)
	}
	period.IssuedAt = period.IssuedAt.Add(time.Minute)
	cache.byStation["KSFO"] = stationWeatherForecast{availableAt: now, periods: []feeds.TAFPeriod{period}}
	reissue := flight
	reissue.ObservedAt = now.Add(time.Nanosecond)
	if err := store.observeTracked(t.Context(), reissue, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if details, err := store.Details(t.Context(), flight.ID); err != nil || details.Prediction == nil || delayAlerts(details) != 1 {
		t.Fatalf("same weather warning was repeated or lost: %+v (%v)", details, err)
	}
	// An amended forecast may retain passenger context but remove the model
	// input. It retires the learned prediction before the next flight poll.
	period.Qualifier, period.Probability = "temporary", 30
	cache.byStation["KSFO"] = stationWeatherForecast{availableAt: now, periods: []feeds.TAFPeriod{period}}
	if details, err := store.Details(t.Context(), flight.ID); err != nil || details.Prediction != nil {
		t.Fatalf("amended weather left a prior prediction: %+v (%v)", details.Prediction, err)
	}
	actual := departure.Add(35 * time.Minute)
	for _, snapshot := range []feeds.Flight{earlier, flight} {
		snapshot.Status, snapshot.ActualDeparture, snapshot.ObservedAt = "taxi_out", &actual, actual.Add(time.Minute)
		if err := store.Observe(t.Context(), snapshot, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := ExportDelaySamples(t.Context(), db, &output); err != nil {
		t.Fatal(err)
	}
	rows, err := predict.ReadExamples(&output)
	if err != nil || len(rows) != 2 || rows[0].Features.TerminalWeatherForecast || !rows[1].Features.TerminalWeatherForecast ||
		!rows[0].ObservedAt.Equal(earlier.ObservedAt) || !rows[1].ObservedAt.Equal(now) {
		t.Fatalf("exported features were not frozen at decision time: %+v (%v)", rows, err)
	}
}

func fitTerminalWeatherFixture(t *testing.T, now time.Time) *predict.Model {
	t.Helper()
	rows := make([]predict.Example, 64)
	for i := range rows {
		observed := now.Add(time.Duration(i-96) * 6 * time.Hour)
		departure := observed.Add(time.Duration(1+20*(i%6)) * time.Minute)
		delay := time.Duration(i%2) * 40 * time.Minute
		rows[i] = predict.Example{
			FlightID: fmt.Sprintf("fixture-%d", i), ObservedAt: observed, ScheduledDeparture: departure, DepartedAt: departure.Add(delay),
			Features: predict.Features{TerminalWeatherForecast: delay > 0},
		}
	}
	model, _, err := predict.Fit(rows, predict.Options{HoldoutFraction: .2, Ridge: .02})
	if err != nil || !model.UsesTerminalWeatherForecast() {
		t.Fatalf("weather fit unavailable: %v", err)
	}
	return model
}
