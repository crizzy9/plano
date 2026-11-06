// Package predict trains and applies a bounded, interpretable departure-delay model.
package predict

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

const (
	baselineSchemaVersion        = 1
	terminalWeatherSchemaVersion = 2
	maxDelayMinutes              = 24 * 60
	minLeadMinutes               = -120
	maxLeadMinutes               = 25 * 60
	minTrainingRows              = 24
	minTestRows                  = 8
	maxRows                      = 100_000
	maxArtifactSize              = 64 << 10
	leadTimeIndex                = 6
	terminalWeatherIndex         = 7
	baselineFeatureCount         = terminalWeatherIndex
)

var featureNames = [...]string{
	"airline_delay", "inbound_turnaround", "airport_delay", "ground_stop", "weather", "air_traffic_control", "lead_time",
	"terminal_weather_forecast",
}

const featureCount = len(featureNames)

// Features holds only information available at the forecast observation time.
// The delay fields express minutes later than the published departure schedule.
type Features struct {
	AirlineDelayMinutes     float64 `json:"airlineDelayMinutes"`
	InboundDelayMinutes     float64 `json:"inboundDelayMinutes"`
	AirportDelayMinutes     float64 `json:"airportDelayMinutes"`
	GroundStopMinutes       float64 `json:"groundStopMinutes"`
	Weather                 bool    `json:"weather"`
	AirTrafficControl       bool    `json:"airTrafficControl"`
	TerminalWeatherForecast bool    `json:"terminalWeatherForecast,omitempty"`
}

func (f Features) values(leadMinutes float64) ([featureCount]float64, bool) {
	values := [featureCount]float64{f.AirlineDelayMinutes, f.InboundDelayMinutes, f.AirportDelayMinutes, f.GroundStopMinutes, 0, 0, leadMinutes}
	if f.Weather {
		values[4] = 1
	}
	if f.AirTrafficControl {
		values[5] = 1
	}
	if f.TerminalWeatherForecast {
		values[terminalWeatherIndex] = 1
	}
	for _, value := range values[:4] {
		if !within(value, 0, maxDelayMinutes) {
			return values, false
		}
	}
	return values, within(leadMinutes, minLeadMinutes, maxLeadMinutes)
}

func within(value, low, high float64) bool {
	return value >= low && value <= high
}

func (f Features) physicalFloor() float64 {
	return max(f.InboundDelayMinutes, f.GroundStopMinutes)
}

func (f Features) operationalBaseline() float64 {
	return max(f.physicalFloor(), f.AirlineDelayMinutes, f.AirportDelayMinutes)
}

func (f Features) hasSignal(useTerminalWeather bool) bool {
	return f.operationalBaseline() > 0 || f.Weather || f.AirTrafficControl || useTerminalWeather && f.TerminalWeatherForecast
}

// ElapsedDepartureDelay is the delay already observed on a flight awaiting
// gate-out at statusAt. An absent predeparture observation supplies no floor.
func ElapsedDepartureDelay(scheduledDeparture, statusAt time.Time) float64 {
	if scheduledDeparture.IsZero() || statusAt.IsZero() {
		return 0
	}
	return max(0, statusAt.Sub(scheduledDeparture).Minutes())
}

type term struct {
	Feature     string  `json:"feature"`
	Mean        float64 `json:"mean"`
	Scale       float64 `json:"scale"`
	Coefficient float64 `json:"coefficient"`
	Min         float64 `json:"min"`
	Max         float64 `json:"max"`
}

type artifact struct {
	SchemaVersion int       `json:"schemaVersion"`
	TrainingRows  int       `json:"trainingRows"`
	TrainingUntil time.Time `json:"trainingUntil"`
	Ridge         float64   `json:"ridge"`
	Intercept     float64   `json:"intercept"`
	Terms         []term    `json:"terms"`
}

// Model is an immutable artifact validated on construction or load.
type Model struct {
	artifact artifact
	id       string
}

func newModel(value artifact) (*Model, error) {
	var expectedTerms int
	switch value.SchemaVersion {
	case baselineSchemaVersion:
		expectedTerms = baselineFeatureCount
	case terminalWeatherSchemaVersion:
		expectedTerms = featureCount
	default:
		return nil, errors.New("unsupported delay model artifact schema")
	}
	if value.TrainingRows < minTrainingRows || value.TrainingRows > maxRows || value.TrainingUntil.IsZero() ||
		!validRidge(value.Ridge) || !within(value.Intercept, 0, maxDelayMinutes) || len(value.Terms) != expectedTerms {
		return nil, errors.New("invalid delay model artifact metadata")
	}
	for i, t := range value.Terms {
		low, high := float64(0), float64(maxDelayMinutes)
		switch i {
		case 4, 5:
			high = 1
		case terminalWeatherIndex:
			high = 1
			if t.Min != 0 || t.Max != 1 {
				return nil, errors.New("terminal weather term must support both forecast states")
			}
		case leadTimeIndex:
			low, high = minLeadMinutes, maxLeadMinutes
		}
		if t.Feature != featureNames[i] || !within(t.Min, low, high) || !within(t.Max, t.Min, high) ||
			!within(t.Mean, t.Min-1e-8, t.Max+1e-8) || !within(t.Scale, 1e-8, high-low+1) || !within(t.Coefficient, -10*maxDelayMinutes, 10*maxDelayMinutes) {
			return nil, fmt.Errorf("invalid delay model term %q", featureNames[i])
		}
	}
	value.TrainingUntil = value.TrainingUntil.UTC()
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	id := fmt.Sprintf("delay-ridge-v%d-%s", value.SchemaVersion, hex.EncodeToString(digest[:8]))
	return &Model{artifact: value, id: id}, nil
}

func validRidge(value float64) bool { return within(value, 1e-6, 100) }

// Load reads a versioned model artifact produced by Save.
func Load(input io.Reader) (*Model, error) {
	body, err := io.ReadAll(io.LimitReader(input, maxArtifactSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxArtifactSize {
		return nil, errors.New("delay model exceeds 64 KiB")
	}
	var value artifact
	if err := decodeJSON(bytes.NewReader(body), &value); err != nil {
		return nil, fmt.Errorf("decode delay model: %w", err)
	}
	return newModel(value)
}

func (m *Model) Save(output io.Writer) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(m.artifact)
}

func (m *Model) ID() string { return m.id }

// UsesTerminalWeatherForecast reports whether the fitted artifact includes TAF.
func (m *Model) UsesTerminalWeatherForecast() bool {
	return m != nil && m.artifact.SchemaVersion == terminalWeatherSchemaVersion
}

// MaxLeadMinutes reports the earliest forecast lead supported by the fit.
func (m *Model) MaxLeadMinutes() float64 {
	if m == nil || m.id == "" {
		return 0
	}
	return m.artifact.Terms[leadTimeIndex].Max
}

// Contribution is a signed change from the historical training baseline.
type Contribution struct {
	Feature string  `json:"feature"`
	Minutes float64 `json:"minutes"`
}

type Estimate struct {
	DelayMinutes  float64
	Baseline      float64
	Contributions []Contribution
}

// Estimate declines extrapolation beyond the feature ranges seen at training.
// It also declines flights with no positive signal supported by the artifact.
func (m *Model) Estimate(features Features, observedAt, scheduledDeparture time.Time) (Estimate, bool) {
	if m == nil || m.id == "" || !observedAt.After(m.artifact.TrainingUntil) || scheduledDeparture.IsZero() ||
		!features.hasSignal(m.UsesTerminalWeatherForecast()) {
		return Estimate{}, false
	}
	values, valid := features.values(scheduledDeparture.Sub(observedAt).Minutes())
	if !valid {
		return Estimate{}, false
	}
	result := Estimate{Baseline: m.artifact.Intercept, Contributions: make([]Contribution, 0, len(m.artifact.Terms)+1)}
	delay := result.Baseline
	for i, t := range m.artifact.Terms {
		value := values[i]
		if !within(value, t.Min-1e-8, t.Max+1e-8) {
			return Estimate{}, false
		}
		minutes := t.Coefficient * (value - t.Mean) / t.Scale
		delay += minutes
		result.Contributions = append(result.Contributions, Contribution{Feature: t.Feature, Minutes: minutes})
	}
	result.DelayMinutes = min(max(delay, features.physicalFloor()), maxDelayMinutes)
	if difference := result.DelayMinutes - delay; math.Abs(difference) > 1e-8 {
		result.Contributions = append(result.Contributions, Contribution{Feature: "operational_constraint", Minutes: difference})
	}
	return result, true
}

func decodeJSON(reader io.Reader, output any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON object")
	}
	return nil
}
