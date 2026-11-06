package predict

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func syntheticExamples() []Example {
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]Example, 80)
	for i := range rows {
		observed := start.Add(time.Duration(i) * 6 * time.Hour)
		lead := 30 + 10*(i%3)
		features := Features{
			AirlineDelayMinutes: float64(5 * (i % 4)), InboundDelayMinutes: float64(10 * (i % 5)),
			AirportDelayMinutes: float64(5 * (i % 7)), GroundStopMinutes: float64(10 * (i % 2)),
			Weather: i%6 >= 3, AirTrafficControl: i%8 >= 4,
		}
		delay := 12 + features.AirlineDelayMinutes + features.InboundDelayMinutes + features.AirportDelayMinutes + features.GroundStopMinutes + 0.2*float64(lead)
		if features.Weather {
			delay += 15
		}
		if features.AirTrafficControl {
			delay += 10
		}
		scheduled := observed.Add(time.Duration(lead) * time.Minute)
		rows[i] = Example{
			FlightID: fmt.Sprintf("synthetic-%03d", i), ObservedAt: observed, ScheduledDeparture: scheduled,
			DepartedAt: scheduled.Add(time.Duration(delay) * time.Minute), Features: features,
		}
	}
	return rows
}

func TestFitChronologicalHoldoutAndDeterministicArtifact(t *testing.T) {
	rows := syntheticExamples()
	boundary := rows[64].ObservedAt
	// Gate-out happened before the split, but the feed reported it after.
	rows[63].LabelObservedAt = new(boundary.Add(40 * time.Minute))
	lastAvailable := boundary.Add(-10 * time.Minute)
	rows[62].LabelObservedAt = &lastAvailable
	options := Options{HoldoutFraction: 0.2, Ridge: 0.01}
	model, result, err := Fit(rows, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.TrainingRows != 63 || result.HoldoutRows != 16 || result.PurgedRows != 1 || !result.HoldoutFrom.Equal(boundary) || !result.TrainingUntil.Equal(lastAvailable) {
		t.Fatalf("chronological boundary = %+v", result)
	}
	if result.LearnedRows != 16 || result.LearnedOrFallback.MeanAbsoluteMinutes > 1 || result.LearnedOrFallback.MeanAbsoluteMinutes >= result.Operational.MeanAbsoluteMinutes {
		t.Fatalf("synthetic recovery = %+v", result)
	}
	var artifact bytes.Buffer
	if err := model.Save(&artifact); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(bytes.NewReader(artifact.Bytes()))
	if err != nil || loaded.ID() != model.ID() {
		t.Fatalf("artifact roundtrip error = %v", err)
	}
	slices.Reverse(rows)
	retrained, again, err := Fit(rows, options)
	if err != nil || retrained.ID() != model.ID() || again != result {
		t.Fatalf("reordered fit = %+v, %v", again, err)
	}
	var secondArtifact bytes.Buffer
	if err := retrained.Save(&secondArtifact); err != nil || !bytes.Equal(artifact.Bytes(), secondArtifact.Bytes()) {
		t.Fatalf("model artifact changed after row reordering: %v", err)
	}
	if _, err := Load(strings.NewReader(strings.Replace(artifact.String(), `"schemaVersion": 1`, `"schemaVersion": 2`, 1))); err == nil {
		t.Fatal("accepted incompatible artifact")
	}
	if _, err := Load(strings.NewReader(artifact.String() + `{}`)); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

func TestEvaluateClampsModelAndOperationalBaselineToElapsedDelay(t *testing.T) {
	rows := syntheticExamples()
	for i := range rows {
		row := &rows[i]
		elapsed := time.Duration(5*(i%10+1)) * time.Minute
		row.ScheduledDeparture = row.ObservedAt.Add(-elapsed)
		row.DepartedAt = row.ObservedAt.Add(5 * time.Minute)
		row.Features = Features{AirlineDelayMinutes: 5}
	}
	model, _, err := Fit(rows, Options{HoldoutFraction: 0.2, Ridge: 100})
	if err != nil {
		t.Fatal(err)
	}
	later := slices.Clone(rows[64:])
	for i := range later {
		row := &later[i]
		row.ScheduledDeparture = row.ObservedAt.Add(-50 * time.Minute)
		estimate, ok := model.Estimate(row.Features, row.ObservedAt, row.ScheduledDeparture)
		if !ok || estimate.DelayMinutes >= 50 {
			t.Fatalf("fixture must exercise a learned estimate below the elapsed delay: %+v, %t", estimate, ok)
		}
	}
	report, err := Evaluate(model, later)
	if err != nil || report.LearnedRows != len(later) || report.Operational.MeanAbsoluteMinutes != 5 ||
		report.LearnedOrFallback.MeanAbsoluteMinutes != 5 || report.Published.MeanAbsoluteMinutes != 50 {
		t.Fatalf("elapsed-aware evaluation = %+v, %v", report, err)
	}
}

func TestEstimateContributionsAndUnsupportedFeatures(t *testing.T) {
	rows := syntheticExamples()
	model, _, err := Fit(rows, Options{HoldoutFraction: 0.2, Ridge: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	row := rows[73]
	got, ok := model.Estimate(row.Features, row.ObservedAt, row.ScheduledDeparture)
	if !ok || got.DelayMinutes < row.Features.physicalFloor() || len(got.Contributions) < baselineFeatureCount {
		t.Fatalf("estimate = %+v, %t", got, ok)
	}
	sum := got.Baseline
	for _, term := range got.Contributions {
		sum += term.Minutes
	}
	if math.Abs(sum-got.DelayMinutes) > 1e-6 {
		t.Fatalf("attribution sum %.4f != estimate %.4f", sum, got.DelayMinutes)
	}
	row.Features.GroundStopMinutes = 120
	if _, ok := model.Estimate(row.Features, row.ObservedAt, row.ScheduledDeparture); ok {
		t.Fatal("extrapolated unseen ground stop duration")
	}
	row = rows[0]
	if _, ok := model.Estimate(row.Features, row.ObservedAt, row.ScheduledDeparture); ok {
		t.Fatal("applied model before training outcomes were observable")
	}
	row = rows[73]
	if _, ok := model.Estimate(Features{}, row.ObservedAt, row.ScheduledDeparture); ok {
		t.Fatal("forecast without a usable observed signal")
	}
}

func TestFitUsesTerminalWeatherOnlyWhenItVariesInFittedRows(t *testing.T) {
	rows := syntheticExamples()
	rows[63].LabelObservedAt = new(rows[64].ObservedAt.Add(time.Hour))
	options := Options{HoldoutFraction: 0.2, Ridge: 0.01}
	baseline, baselineReport, err := Fit(rows, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		forecastAt func(int) bool
	}{
		{"variation only outside fit", func(i int) bool { return i >= 63 }},
		{"constant positive flag", func(_ int) bool { return true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples := slices.Clone(rows)
			for i := range samples {
				samples[i].Features.TerminalWeatherForecast = tc.forecastAt(i)
			}
			model, report, err := Fit(samples, options)
			if err != nil {
				t.Fatal(err)
			}
			if model.UsesTerminalWeatherForecast() || model.ID() != baseline.ID() || report != baselineReport {
				t.Fatalf("fit without terminal weather variation changed the baseline: %+v", report)
			}
		})
	}
	row := rows[73]
	without, supported := baseline.Estimate(row.Features, row.ObservedAt, row.ScheduledDeparture)
	row.Features.TerminalWeatherForecast = true
	with, alsoSupported := baseline.Estimate(row.Features, row.ObservedAt, row.ScheduledDeparture)
	if !supported || !alsoSupported || with.DelayMinutes != without.DelayMinutes || !slices.Equal(with.Contributions, without.Contributions) {
		t.Fatalf("v1 estimate changed with terminal weather: %+v vs %+v", without, with)
	}
	if _, ok := baseline.Estimate(Features{TerminalWeatherForecast: true}, row.ObservedAt, row.ScheduledDeparture); ok {
		t.Fatal("v1 model treated terminal weather alone as a supported signal")
	}
}

func TestTerminalWeatherModelRoundTripAndBacktest(t *testing.T) {
	rows := syntheticExamples()
	for i := range rows {
		rows[i].Features = Features{TerminalWeatherForecast: i%2 == 0}
		rows[i].DepartedAt = rows[i].ScheduledDeparture
		if rows[i].Features.TerminalWeatherForecast {
			rows[i].DepartedAt = rows[i].DepartedAt.Add(30 * time.Minute)
		}
	}
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	samples, err := ReadExamples(&input)
	if err != nil || len(samples) != len(rows) || samples[0].Features != rows[0].Features || samples[1].Features != rows[1].Features {
		t.Fatalf("terminal weather JSONL round trip failed: %v", err)
	}
	model, report, err := Fit(samples, Options{HoldoutFraction: 0.2, Ridge: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	if !model.UsesTerminalWeatherForecast() || !strings.HasPrefix(model.ID(), "delay-ridge-v2-") || model.MaxLeadMinutes() != 50 {
		t.Fatalf("unexpected terminal weather artifact %s, maximum lead %v", model.ID(), model.MaxLeadMinutes())
	}
	if report.EligibleRows != 8 || report.LearnedRows != 8 || report.EligibleCoverage != 1 || report.LearnedOrFallback.MeanAbsoluteMinutes > 1 {
		t.Fatalf("terminal weather backtest = %+v", report)
	}
	weather := report.TerminalWeather
	if weather == nil || weather.HoldoutRows != 8 || weather.LearnedRows != 8 || weather.LearnedCoverage != 1 ||
		weather.LearnedOrFallback == nil || weather.LearnedOrFallback.MeanAbsoluteMinutes > 1 {
		t.Fatalf("weather-bearing holdout = %+v", weather)
	}
	paired := weather.WithoutTerminalWeather
	if paired == nil || paired.AllFlights.MeanAbsoluteMinutes != 15 ||
		paired.WeatherFlights == nil || paired.WeatherFlights.MeanAbsoluteMinutes != 30 {
		t.Fatalf("paired non-TAF baseline = %+v", paired)
	}
	var artifact bytes.Buffer
	if err := model.Save(&artifact); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(bytes.NewReader(artifact.Bytes()))
	if err != nil || loaded.ID() != model.ID() || !loaded.UsesTerminalWeatherForecast() {
		t.Fatalf("terminal weather artifact round trip failed: %v", err)
	}
	review, err := Evaluate(loaded, rows[64:])
	expected := report
	weatherOnly := *weather
	weatherOnly.WithoutTerminalWeather = nil
	expected.TerminalWeather = &weatherOnly
	if err != nil || !reflect.DeepEqual(review.Backtest, expected) {
		t.Fatalf("loaded terminal weather evaluation = %+v, %v", review, err)
	}
	row := rows[72]
	estimate, ok := loaded.Estimate(row.Features, row.ObservedAt, row.ScheduledDeparture)
	if !ok || math.Abs(estimate.DelayMinutes-30) > 1 || len(estimate.Contributions) != featureCount {
		t.Fatalf("terminal weather-only estimate = %+v, %t", estimate, ok)
	}
	contribution := estimate.Contributions[terminalWeatherIndex]
	if contribution.Feature != "terminal_weather_forecast" || contribution.Minutes <= 0 {
		t.Fatalf("terminal weather contribution = %+v", contribution)
	}
	if _, ok := loaded.Estimate(row.Features, row.ObservedAt, row.ObservedAt.Add(51*time.Minute)); ok {
		t.Fatal("terminal weather model extrapolated beyond its supported lead time")
	}
}

func TestFitRejectsDuplicateFlightsAndLeakedLabels(t *testing.T) {
	rows := syntheticExamples()
	options := Options{HoldoutFraction: 0.2, Ridge: 0.01}
	rows[67].FlightID = rows[0].FlightID
	if _, _, err := Fit(rows, options); err == nil || !strings.Contains(err.Error(), "repeated flight ID") {
		t.Fatalf("duplicate flight error = %v", err)
	}
	rows = syntheticExamples()
	rows[67].DepartedAt = rows[67].ObservedAt
	if _, _, err := Fit(rows, options); err == nil || !strings.Contains(err.Error(), "invalid timestamps") {
		t.Fatalf("pre-observation label error = %v", err)
	}
	rows = syntheticExamples()
	rows[67].LabelObservedAt = new(rows[67].DepartedAt.Add(-time.Minute))
	if _, _, err := Fit(rows, options); err == nil || !strings.Contains(err.Error(), "invalid timestamps") {
		t.Fatalf("label observation before gate-out error = %v", err)
	}
	if _, err := ReadExamples(strings.NewReader(`{"unknown":true}`)); err == nil {
		t.Fatal("accepted an unknown row field")
	}
}

func TestEvaluateUsesLatestFittedLabelAsStrictCutoff(t *testing.T) {
	rows := syntheticExamples()
	boundary := rows[64].ObservedAt
	rows[63].LabelObservedAt = new(boundary.Add(30 * time.Minute))
	rows[62].LabelObservedAt = new(boundary.Add(-time.Minute))
	model, fitted, err := Fit(rows, Options{HoldoutFraction: 0.2, Ridge: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	equal := rows[64]
	equal.FlightID = "at-cutoff"
	equal.ObservedAt = fitted.TrainingUntil
	rows = append(rows, equal)
	got, err := Evaluate(model, rows)
	if err != nil {
		t.Fatal(err)
	}
	fitted.PurgedRows = 0
	if got.ExcludedThroughTraining != 65 || got.Backtest != fitted {
		t.Fatalf("evaluation = %+v, want %d excluded and %+v", got, 65, fitted)
	}
	slices.Reverse(rows)
	if reversed, err := Evaluate(model, rows); err != nil || reversed != got {
		t.Fatalf("evaluation changed after row reordering: %+v, %v", reversed, err)
	}
}

func TestEvaluateKeepsOperationalFallbackAndRejectsInvalidRows(t *testing.T) {
	rows := syntheticExamples()
	model, _, err := Fit(rows, Options{HoldoutFraction: 0.2, Ridge: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	later := rows[72:]
	for i := range later {
		later[i].Features = Features{AirlineDelayMinutes: 15, GroundStopMinutes: 120}
		later[i].DepartedAt = later[i].ScheduledDeparture.Add(90 * time.Minute)
	}
	got, err := Evaluate(model, later)
	if err != nil || got.ExcludedThroughTraining != 0 || got.HoldoutRows != 8 || got.LearnedCoverage != 0 ||
		got.EligibleRows != 8 || got.EligibleCoverage != 0 ||
		got.LearnedOrFallback != (ErrorMetrics{30, 30, 30}) || got.Operational != got.LearnedOrFallback ||
		got.Published != (ErrorMetrics{75, 75, -75}) {
		t.Fatalf("fallback evaluation = %+v, %v", got, err)
	}
	if _, err := Evaluate(model, later[:7]); err == nil || !strings.Contains(err.Error(), "at least 8") {
		t.Fatalf("short evaluation error = %v", err)
	}
	if _, err := Evaluate(nil, later); err == nil || !strings.Contains(err.Error(), "requires a fitted model") {
		t.Fatalf("missing model error = %v", err)
	}
	later[0].FlightID = later[1].FlightID
	if _, err := Evaluate(model, later); err == nil || !strings.Contains(err.Error(), "repeated flight ID") {
		t.Fatalf("duplicate flight error = %v", err)
	}
}
