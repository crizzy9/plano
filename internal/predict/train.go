package predict

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

// Example is one forecast snapshot and its subsequently observed gate-out time.
// FlightID must be unique in a training dataset to keep flight instances isolated.
type Example struct {
	FlightID           string     `json:"flightId"`
	ObservedAt         time.Time  `json:"observedAt"`
	ScheduledDeparture time.Time  `json:"scheduledDeparture"`
	DepartedAt         time.Time  `json:"departedAt"`
	LabelObservedAt    *time.Time `json:"labelObservedAt,omitempty"`
	Features           Features   `json:"features"`
}

func ReadExamples(reader io.Reader) ([]Example, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 32<<10)
	rows := make([]Example, 0)
	for line := 1; scanner.Scan(); line++ {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		if len(rows) == maxRows {
			return nil, fmt.Errorf("training data exceeds %d examples", maxRows)
		}
		var row Example
		if err := decodeJSON(bytes.NewReader(raw), &row); err != nil {
			return nil, fmt.Errorf("invalid training JSON at line %d: %w", line, err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read training data: %w", err)
	}
	return rows, nil
}

type Options struct {
	HoldoutFraction float64
	Ridge           float64
}

type ErrorMetrics struct {
	MeanAbsoluteMinutes float64 `json:"maeMinutes"`
	RootMeanSqMinutes   float64 `json:"rmseMinutes"`
	BiasMinutes         float64 `json:"biasMinutes"`
}

type Backtest struct {
	ModelVersion      string                   `json:"modelVersion"`
	TrainingRows      int                      `json:"trainingRows"`
	TrainingUntil     time.Time                `json:"trainingUntil"`
	HoldoutFrom       time.Time                `json:"holdoutFrom"`
	HoldoutRows       int                      `json:"holdoutRows"`
	PurgedRows        int                      `json:"purgedRows"`
	LearnedRows       int                      `json:"learnedRows"`
	LearnedCoverage   float64                  `json:"learnedCoverage"`
	EligibleRows      int                      `json:"eligibleRows"`
	EligibleCoverage  float64                  `json:"eligibleCoverage"`
	Operational       ErrorMetrics             `json:"operationalBaseline"`
	Published         ErrorMetrics             `json:"publishedEstimateBaseline"`
	LearnedOrFallback ErrorMetrics             `json:"learnedOrFallback"`
	TerminalWeather   *TerminalWeatherBacktest `json:"terminalWeather,omitempty"`
}

// TerminalWeatherBacktest scores the held-out flights with adverse TAF evidence.
type TerminalWeatherBacktest struct {
	HoldoutRows            int                      `json:"holdoutRows"`
	LearnedRows            int                      `json:"learnedRows"`
	LearnedCoverage        float64                  `json:"learnedCoverage"`
	LearnedOrFallback      *ErrorMetrics            `json:"learnedOrFallback,omitempty"`
	WithoutTerminalWeather *TerminalWeatherBaseline `json:"withoutTerminalWeather,omitempty"`
}

// TerminalWeatherBaseline scores a fit that uses the same examples and ridge
// penalty with the terminal weather term excluded.
type TerminalWeatherBaseline struct {
	AllFlights     ErrorMetrics  `json:"allFlights"`
	WeatherFlights *ErrorMetrics `json:"weatherFlights,omitempty"`
}

type Evaluation struct {
	Backtest
	ExcludedThroughTraining int `json:"excludedThroughTraining"`
}

type trainingPoint struct {
	features [featureCount]float64
	label    float64
}

func (e Example) labelAvailableAt() time.Time {
	if e.LabelObservedAt != nil {
		return *e.LabelObservedAt
	}
	return e.DepartedAt
}

func (e Example) point() (trainingPoint, bool) {
	features, ok := e.Features.values(e.ScheduledDeparture.Sub(e.ObservedAt).Minutes())
	delay := max(0, e.DepartedAt.Sub(e.ScheduledDeparture).Minutes())
	valid := ok && !e.ObservedAt.IsZero() && !e.ScheduledDeparture.IsZero() && e.DepartedAt.After(e.ObservedAt) &&
		!e.labelAvailableAt().Before(e.DepartedAt) && within(delay, 0, maxDelayMinutes)
	return trainingPoint{features: features, label: delay}, valid
}

// Fit trains standardized ridge regression and tests it against later forecasts.
// A flight's label must already have been observable at the holdout boundary.
func Fit(examples []Example, options Options) (*Model, Backtest, error) {
	if len(examples) < minTrainingRows+minTestRows || len(examples) > maxRows {
		return nil, Backtest{}, fmt.Errorf("provide between %d and %d historical flight examples", minTrainingRows+minTestRows, maxRows)
	}
	if !within(options.HoldoutFraction, 0.1, 0.5) || !validRidge(options.Ridge) {
		return nil, Backtest{}, errors.New("invalid holdout fraction or ridge penalty")
	}
	if err := validateExamples(examples); err != nil {
		return nil, Backtest{}, err
	}
	rows := slices.Clone(examples)
	sortExamples(rows)
	holdoutSize := max(minTestRows, int(math.Ceil(float64(len(rows))*options.HoldoutFraction)))
	boundary := rows[len(rows)-holdoutSize].ObservedAt.UTC()
	training := make([]trainingPoint, 0, len(rows)-holdoutSize)
	holdout := make([]Example, 0, holdoutSize)
	var latestLabel time.Time
	purged := 0
	for _, row := range rows {
		switch {
		case !row.ObservedAt.Before(boundary):
			holdout = append(holdout, row)
		case !row.labelAvailableAt().Before(boundary):
			purged++
		default:
			point, _ := row.point()
			training = append(training, point)
			if available := row.labelAvailableAt(); available.After(latestLabel) {
				latestLabel = available
			}
		}
	}
	if len(training) < minTrainingRows {
		return nil, Backtest{}, errors.New("not enough earlier flights with outcomes known before the holdout")
	}
	model, err := train(training, latestLabel, options.Ridge, true)
	if err != nil {
		return nil, Backtest{}, err
	}
	var withoutTerminalWeather *Model
	if model.UsesTerminalWeatherForecast() {
		withoutTerminalWeather, err = train(training, latestLabel, options.Ridge, false)
		if err != nil {
			return nil, Backtest{}, err
		}
	}
	return model, backtest(model, withoutTerminalWeather, holdout, boundary, purged), nil
}

// Evaluate scores an existing model on observations after its latest fitted label.
func Evaluate(model *Model, examples []Example) (Evaluation, error) {
	if model == nil || model.id == "" {
		return Evaluation{}, errors.New("evaluation requires a fitted model")
	}
	if len(examples) > maxRows {
		return Evaluation{}, fmt.Errorf("provide at most %d historical flight examples", maxRows)
	}
	if err := validateExamples(examples); err != nil {
		return Evaluation{}, err
	}
	eligible := make([]Example, 0, len(examples))
	for _, row := range examples {
		if row.ObservedAt.After(model.artifact.TrainingUntil) {
			eligible = append(eligible, row)
		}
	}
	if len(eligible) < minTestRows {
		return Evaluation{}, fmt.Errorf("evaluation requires at least %d flights observed after training", minTestRows)
	}
	sortExamples(eligible)
	return Evaluation{
		Backtest:                backtest(model, nil, eligible, eligible[0].ObservedAt.UTC(), 0),
		ExcludedThroughTraining: len(examples) - len(eligible),
	}, nil
}

func validateExamples(examples []Example) error {
	seen := make(map[string]bool, len(examples))
	for i, row := range examples {
		if row.FlightID == "" || len(row.FlightID) > 128 || strings.TrimSpace(row.FlightID) != row.FlightID || seen[row.FlightID] {
			return fmt.Errorf("invalid or repeated flight ID at example %d", i+1)
		}
		if _, valid := row.point(); !valid {
			return fmt.Errorf("invalid timestamps, feature values, or label at example %d", i+1)
		}
		seen[row.FlightID] = true
	}
	return nil
}

func sortExamples(rows []Example) {
	slices.SortFunc(rows, func(a, b Example) int {
		if order := a.ObservedAt.Compare(b.ObservedAt); order != 0 {
			return order
		}
		return strings.Compare(a.FlightID, b.FlightID)
	})
}

func train(points []trainingPoint, through time.Time, ridge float64, allowTerminalWeather bool) (*Model, error) {
	schema, termCount := baselineSchemaVersion, baselineFeatureCount
	firstWeather := points[0].features[terminalWeatherIndex]
	if allowTerminalWeather && slices.ContainsFunc(points[1:], func(point trainingPoint) bool {
		return point.features[terminalWeatherIndex] != firstWeather
	}) {
		schema, termCount = terminalWeatherSchemaVersion, featureCount
	}
	terms := make([]term, termCount)
	count := float64(len(points))
	for i, name := range featureNames[:termCount] {
		terms[i] = term{Feature: name, Min: points[0].features[i], Max: points[0].features[i]}
	}
	var meanLabel float64
	for _, point := range points {
		meanLabel += point.label / count
		for i, value := range point.features[:termCount] {
			terms[i].Mean += value / count
			terms[i].Min = min(terms[i].Min, value)
			terms[i].Max = max(terms[i].Max, value)
		}
	}
	for _, point := range points {
		for i, value := range point.features[:termCount] {
			delta := value - terms[i].Mean
			terms[i].Scale += delta * delta / count
		}
	}
	for i := range terms {
		terms[i].Scale = math.Sqrt(terms[i].Scale)
		if terms[i].Scale < 1e-8 {
			terms[i].Scale = 1
		}
	}
	var gram [featureCount][featureCount]float64
	var rhs [featureCount]float64
	for _, point := range points {
		var standardized [featureCount]float64
		for i, value := range point.features[:termCount] {
			standardized[i] = (value - terms[i].Mean) / terms[i].Scale
		}
		for i, value := range standardized[:termCount] {
			rhs[i] += value * (point.label - meanLabel) / count
			for j := range i + 1 {
				gram[i][j] += value * standardized[j] / count
			}
		}
	}
	for i := range termCount {
		gram[i][i] += ridge
	}
	weights, err := solvePositiveDefinite(gram, rhs, termCount)
	if err != nil {
		return nil, err
	}
	for i, weight := range weights[:termCount] {
		terms[i].Coefficient = weight
	}
	return newModel(artifact{SchemaVersion: schema, TrainingRows: len(points), TrainingUntil: through, Ridge: ridge, Intercept: min(meanLabel, maxDelayMinutes), Terms: terms})
}

// Cholesky solves the positive-definite ridge normal equations in fixed memory.
func solvePositiveDefinite(matrix [featureCount][featureCount]float64, rhs [featureCount]float64, size int) ([featureCount]float64, error) {
	var lower [featureCount][featureCount]float64
	for i := range size {
		for j := range i + 1 {
			value := matrix[i][j]
			for k := range j {
				value -= lower[i][k] * lower[j][k]
			}
			if i == j {
				if value <= 0 || math.IsNaN(value) {
					return rhs, errors.New("delay model system is not positive definite")
				}
				lower[i][i] = math.Sqrt(value)
			} else {
				lower[i][j] = value / lower[j][j]
			}
		}
	}
	for i := range size {
		for j := range i {
			rhs[i] -= lower[i][j] * rhs[j]
		}
		rhs[i] /= lower[i][i]
	}
	for i := size - 1; i >= 0; i-- {
		for j := i + 1; j < size; j++ {
			rhs[i] -= lower[j][i] * rhs[j]
		}
		rhs[i] /= lower[i][i]
	}
	return rhs, nil
}

func backtest(model, withoutTerminalWeather *Model, holdout []Example, boundary time.Time, purged int) Backtest {
	result := Backtest{
		ModelVersion: model.ID(), TrainingRows: model.artifact.TrainingRows, TrainingUntil: model.artifact.TrainingUntil,
		HoldoutFrom: boundary, HoldoutRows: len(holdout), PurgedRows: purged,
	}
	var weather *TerminalWeatherBacktest
	var paired *TerminalWeatherBaseline
	var weatherErrors, pairedWeatherErrors ErrorMetrics
	if model.UsesTerminalWeatherForecast() {
		weather = &TerminalWeatherBacktest{}
		if withoutTerminalWeather != nil {
			paired = &TerminalWeatherBaseline{}
			weather.WithoutTerminalWeather = paired
		}
		result.TerminalWeather = weather
	}
	for _, row := range holdout {
		point, _ := row.point()
		elapsed := ElapsedDepartureDelay(row.ScheduledDeparture, row.ObservedAt)
		operational := max(row.Features.operationalBaseline(), elapsed)
		prediction, learned := estimateOrFallback(model, row, elapsed, operational)
		if row.Features.hasSignal(model.UsesTerminalWeatherForecast()) {
			result.EligibleRows++
		}
		if learned {
			result.LearnedRows++
		}
		result.Operational.record(operational - point.label)
		result.Published.record(row.Features.AirlineDelayMinutes - point.label)
		result.LearnedOrFallback.record(prediction - point.label)
		hasTerminalWeather := weather != nil && row.Features.TerminalWeatherForecast
		if hasTerminalWeather {
			weather.HoldoutRows++
			if learned {
				weather.LearnedRows++
			}
			weatherErrors.record(prediction - point.label)
		}
		if paired != nil {
			prediction, _ := estimateOrFallback(withoutTerminalWeather, row, elapsed, operational)
			paired.AllFlights.record(prediction - point.label)
			if hasTerminalWeather {
				pairedWeatherErrors.record(prediction - point.label)
			}
		}
	}
	result.LearnedCoverage = float64(result.LearnedRows) / float64(result.HoldoutRows)
	if result.EligibleRows > 0 {
		result.EligibleCoverage = float64(result.LearnedRows) / float64(result.EligibleRows)
	}
	result.Operational.average(result.HoldoutRows)
	result.Published.average(result.HoldoutRows)
	result.LearnedOrFallback.average(result.HoldoutRows)
	if paired != nil {
		paired.AllFlights.average(result.HoldoutRows)
	}
	if weather != nil && weather.HoldoutRows > 0 {
		weather.LearnedCoverage = float64(weather.LearnedRows) / float64(weather.HoldoutRows)
		weatherErrors.average(weather.HoldoutRows)
		weather.LearnedOrFallback = &weatherErrors
		if paired != nil {
			pairedWeatherErrors.average(weather.HoldoutRows)
			paired.WeatherFlights = &pairedWeatherErrors
		}
	}
	return result
}

func estimateOrFallback(model *Model, row Example, elapsed, fallback float64) (float64, bool) {
	if estimate, ok := model.Estimate(row.Features, row.ObservedAt, row.ScheduledDeparture); ok {
		return max(estimate.DelayMinutes, elapsed), true
	}
	return fallback, false
}

func (m *ErrorMetrics) record(errorMinutes float64) {
	m.MeanAbsoluteMinutes += math.Abs(errorMinutes)
	m.RootMeanSqMinutes += errorMinutes * errorMinutes
	m.BiasMinutes += errorMinutes
}

func (m *ErrorMetrics) average(count int) {
	m.MeanAbsoluteMinutes /= float64(count)
	m.RootMeanSqMinutes = math.Sqrt(m.RootMeanSqMinutes / float64(count))
	m.BiasMinutes /= float64(count)
}
