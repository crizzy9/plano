// Package historicalrisk models historical departure delay risk from the BTS
// Reporting Carrier On-Time Performance dataset and scheduled flight inputs.
package historicalrisk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
)

const (
	schemaVersion     = 1
	departureBins     = 8
	minimumBinFlights = 100
	priorFlights      = 100
	maxAirports       = 1500
	maxRoutes         = 40_000
	maxCSVRows        = 3_000_000
	maxArtifactBytes  = 4 << 20
)

type counts struct {
	Flights int `json:"flights"`
	Delayed int `json:"delayed"`
}

func (c counts) rate() float64 {
	return float64(c.Delayed) / float64(c.Flights)
}

type airportArtifact struct {
	Destinations []string `json:"destinations"`
	Departure    []counts `json:"departure"`
}

type artifact struct {
	SchemaVersion   int                        `json:"schemaVersion"`
	PublishedOn     string                     `json:"publishedOn"`
	TrainingFrom    string                     `json:"trainingFrom"`
	TrainingThrough string                     `json:"trainingThrough"`
	Airports        map[string]airportArtifact `json:"airports"`
}

type airport struct {
	airportArtifact
	total counts
}

// Model is a validated, immutable estimate for supported scheduled flights.
type Model struct {
	artifact    artifact
	airports    map[string]airport
	global      counts
	id          string
	availableAt time.Time
	expiresAt   time.Time
}

// Estimate is the historical probability of a departure at least 15 minutes
// late among flights that operated in the same origin and schedule window.
type Estimate struct {
	Probability     float64 `json:"probability"`
	SampleSize      int     `json:"sampleSize"`
	TrainingThrough string  `json:"trainingThrough"`
	ModelVersion    string  `json:"modelVersion"`
}

func newModel(value artifact) (*Model, error) {
	published, err := parseDate(value.PublishedOn)
	if err != nil {
		return nil, errors.New("invalid historical risk publication date")
	}
	if _, err := parseDate(value.TrainingFrom); err != nil {
		return nil, errors.New("invalid historical risk training start date")
	}
	if _, err := parseDate(value.TrainingThrough); err != nil {
		return nil, errors.New("invalid historical risk training end date")
	}
	if value.SchemaVersion != schemaVersion || value.TrainingFrom > value.TrainingThrough ||
		value.TrainingThrough > value.PublishedOn || len(value.Airports) == 0 || len(value.Airports) > maxAirports {
		return nil, errors.New("invalid historical risk model metadata")
	}
	model := &Model{
		artifact: value, airports: make(map[string]airport, len(value.Airports)),
		availableAt: published.AddDate(0, 0, 1), expiresAt: published.AddDate(1, 0, 1),
	}
	routes := 0
	for origin, site := range value.Airports {
		if !airportCode(origin) || len(site.Destinations) == 0 || len(site.Departure) != departureBins {
			return nil, errors.New("invalid airport in historical risk model")
		}
		routes += len(site.Destinations)
		if routes > maxRoutes {
			return nil, errors.New("too many routes in historical risk model")
		}
		for i, dest := range site.Destinations {
			if !airportCode(dest) || dest == origin || (i > 0 && dest <= site.Destinations[i-1]) {
				return nil, errors.New("invalid destination in historical risk model")
			}
		}
		var total counts
		for _, bin := range site.Departure {
			if bin.Flights < 0 || bin.Flights > maxCSVRows || bin.Delayed < 0 || bin.Delayed > bin.Flights {
				return nil, errors.New("invalid departure counts in historical risk model")
			}
			total.Flights += bin.Flights
			total.Delayed += bin.Delayed
		}
		if total.Flights < len(site.Destinations) || total.Flights > maxCSVRows-model.global.Flights {
			return nil, errors.New("inconsistent flight counts in historical risk model")
		}
		model.airports[origin] = airport{airportArtifact: site, total: total}
		model.global.Flights += total.Flights
		model.global.Delayed += total.Delayed
	}
	if model.global.Flights < minimumBinFlights {
		return nil, errors.New("historical risk model needs at least 100 operated flights")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	model.id = "bts-departure-risk-v1-" + hex.EncodeToString(digest[:8])
	return model, nil
}

func airportCode(code string) bool {
	if len(code) != 3 {
		return false
	}
	for i := range len(code) {
		c := code[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// Load reads the bounded, versioned artifact written by Save.
func Load(input io.Reader) (*Model, error) {
	body, err := io.ReadAll(io.LimitReader(input, maxArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxArtifactBytes {
		return nil, errors.New("historical risk model exceeds 4 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var value artifact
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode historical risk model: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected one historical risk model object")
	}
	return newModel(value)
}

// Save writes an artifact that Load can validate without the training data.
func (m *Model) Save(output io.Writer) error {
	if m == nil || m.id == "" {
		return errors.New("no fitted historical risk model")
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(m.artifact)
}

// Estimate uses scheduledLocal's location to select the origin's departure
// window. Only upcoming flights on supported routes receive an estimate.
func (m *Model) Estimate(origin, destination string, scheduledLocal, timeAsOf time.Time) (Estimate, bool) {
	if m == nil || m.id == "" || timeAsOf.Before(m.availableAt) || !timeAsOf.Before(m.expiresAt) ||
		!scheduledLocal.After(timeAsOf) {
		return Estimate{}, false
	}
	scheduledDay := scheduledLocal.Format(time.DateOnly)
	if scheduledDay <= m.artifact.PublishedOn || scheduledDay >= m.expiresAt.Format(time.DateOnly) {
		return Estimate{}, false
	}
	return m.estimate(origin, destination, scheduledLocal.Hour()/3)
}

func (m *Model) estimate(origin, destination string, bin int) (Estimate, bool) {
	site, known := m.airports[origin]
	if !known {
		return Estimate{}, false
	}
	row := site.Departure[bin]
	if _, found := slices.BinarySearch(site.Destinations, destination); !found || row.Flights < minimumBinFlights {
		return Estimate{}, false
	}
	return Estimate{
		Probability: (float64(row.Delayed) + priorFlights*site.total.rate()) / float64(row.Flights+priorFlights),
		SampleSize:  row.Flights, TrainingThrough: m.artifact.TrainingThrough, ModelVersion: m.id,
	}, true
}
