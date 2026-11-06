package historicalrisk

import (
	"errors"
	"io"
	"slices"
	"strings"
	"time"
)

// Fit reduces the streaming BTS rows to airport, route, and schedule counts.
// publishedOn is the date when the latest archive was publicly available.
func Fit(input io.Reader, publishedOn time.Time) (*Model, error) {
	if publishedOn.IsZero() {
		return nil, errors.New("historical risk training requires a publication date")
	}
	type airportTraining struct {
		destinations map[string]struct{}
		departure    []counts
	}
	airports := make(map[string]airportTraining)
	value := artifact{SchemaVersion: schemaVersion, PublishedOn: publishedOn.Format(time.DateOnly)}
	routes := 0
	err := readFlights(input, func(row flightRow) error {
		if row.date > value.PublishedOn {
			return errors.New("flight date is later than the training publication date")
		}
		if value.TrainingFrom == "" || row.date < value.TrainingFrom {
			value.TrainingFrom = strings.Clone(row.date)
		}
		if row.date > value.TrainingThrough {
			value.TrainingThrough = strings.Clone(row.date)
		}
		site, exists := airports[row.origin]
		if !exists {
			if len(airports) == maxAirports {
				return errors.New("too many training airports")
			}
			site = airportTraining{destinations: make(map[string]struct{}), departure: make([]counts, departureBins)}
			airports[strings.Clone(row.origin)] = site
		}
		if _, exists := site.destinations[row.destination]; !exists {
			routes++
			if routes > maxRoutes {
				return errors.New("too many training routes")
			}
			site.destinations[strings.Clone(row.destination)] = struct{}{}
		}
		bin := &site.departure[row.bin]
		bin.Flights++
		if row.delayed {
			bin.Delayed++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(airports) == 0 {
		return nil, errors.New("training input contains no operated flights with departure labels")
	}
	value.Airports = make(map[string]airportArtifact, len(airports))
	for code, site := range airports {
		destinations := make([]string, 0, len(site.destinations))
		for destination := range site.destinations {
			destinations = append(destinations, destination)
		}
		slices.Sort(destinations)
		value.Airports[code] = airportArtifact{Destinations: destinations, Departure: site.departure}
	}
	return newModel(value)
}

type Evaluation struct {
	ModelVersion               string  `json:"modelVersion"`
	TrainingRows               int     `json:"trainingRows"`
	TrainingFrom               string  `json:"trainingFrom"`
	TrainingThrough            string  `json:"trainingThrough"`
	PublishedOn                string  `json:"publishedOn"`
	HoldoutRows                int     `json:"holdoutRows"`
	HoldoutFrom                string  `json:"holdoutFrom"`
	HoldoutThrough             string  `json:"holdoutThrough"`
	ExcludedThroughPublication int     `json:"excludedThroughPublication"`
	ExcludedAfterExpiration    int     `json:"excludedAfterExpiration"`
	LearnedRows                int     `json:"learnedRows"`
	LearnedCoverage            float64 `json:"learnedCoverage"`
	LearnedOrFallbackBrier     float64 `json:"learnedOrFallbackBrier"`
	AirportOnlyBrier           float64 `json:"airportOnlyBrier"`
	GlobalBrier                float64 `json:"globalBrier"`
}

// Evaluate scores a later archive against frozen model, airport, and overall
// probabilities. Unsupported rows use the airport or overall fallback.
func Evaluate(model *Model, input io.Reader) (Evaluation, error) {
	if model == nil || model.id == "" {
		return Evaluation{}, errors.New("evaluation requires a fitted historical risk model")
	}
	result := Evaluation{
		ModelVersion: model.id, TrainingRows: model.global.Flights, TrainingFrom: model.artifact.TrainingFrom,
		TrainingThrough: model.artifact.TrainingThrough, PublishedOn: model.artifact.PublishedOn,
	}
	global := model.global.rate()
	expiresOn := model.expiresAt.Format(time.DateOnly)
	err := readFlights(input, func(row flightRow) error {
		if row.date <= result.PublishedOn {
			result.ExcludedThroughPublication++
			return nil
		}
		if row.date >= expiresOn {
			result.ExcludedAfterExpiration++
			return nil
		}
		result.HoldoutRows++
		if result.HoldoutFrom == "" || row.date < result.HoldoutFrom {
			result.HoldoutFrom = strings.Clone(row.date)
		}
		if row.date > result.HoldoutThrough {
			result.HoldoutThrough = strings.Clone(row.date)
		}
		airport := global
		if site, ok := model.airports[row.origin]; ok {
			airport = site.total.rate()
		}
		learned := airport
		if estimate, ok := model.estimate(row.origin, row.destination, row.bin); ok {
			learned = estimate.Probability
			result.LearnedRows++
		}
		var outcome float64
		if row.delayed {
			outcome = 1
		}
		result.LearnedOrFallbackBrier += squaredError(learned, outcome)
		result.AirportOnlyBrier += squaredError(airport, outcome)
		result.GlobalBrier += squaredError(global, outcome)
		return nil
	})
	if err != nil {
		return Evaluation{}, err
	}
	if result.HoldoutRows < minimumBinFlights {
		return Evaluation{}, errors.New("evaluation needs at least 100 operated flights after publication and before expiration")
	}
	total := float64(result.HoldoutRows)
	result.LearnedCoverage = float64(result.LearnedRows) / total
	result.LearnedOrFallbackBrier /= total
	result.AirportOnlyBrier /= total
	result.GlobalBrier /= total
	return result, nil
}

func squaredError(prediction, outcome float64) float64 {
	difference := prediction - outcome
	return difference * difference
}
