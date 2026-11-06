package core

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const (
	weatherForecastCadence   = 15 * time.Minute
	weatherForecastLead      = 24 * time.Hour
	weatherForecastBatchSize = feeds.MaxTAFStations
	weatherForecastRowLimit  = 4 // Per departure or arrival.
)

type TAFProvider interface {
	Forecasts(context.Context, []string, time.Time) ([]feeds.TAFPeriod, error)
}

type AirportWeatherForecast struct {
	Airport     string    `json:"airport"`
	Operation   string    `json:"operation"`
	Conditions  string    `json:"conditions"`
	IssuedAt    time.Time `json:"issuedAt"`
	StartsAt    time.Time `json:"startsAt"`
	EndsAt      time.Time `json:"endsAt"`
	Qualifier   string    `json:"qualifier"`
	Probability int       `json:"probability,omitempty"`
}

type stationWeatherForecast struct {
	availableAt time.Time
	periods     []feeds.TAFPeriod
}

// WeatherForecastCache holds forecast periods for public stations near
// tracked flight operations. Its worker runs separately from observation polls.
type WeatherForecastCache struct {
	provider  TAFProvider
	mu        sync.RWMutex
	byStation map[string]stationWeatherForecast
	after     string
}

func NewWeatherForecastCache(provider TAFProvider) *WeatherForecastCache {
	return &WeatherForecastCache{provider: provider, byStation: make(map[string]stationWeatherForecast)}
}

func (w *WeatherForecastCache) Run(ctx context.Context, store *Store, logger *slog.Logger) {
	tick := time.NewTicker(weatherForecastCadence)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := w.refresh(ctx, store, time.Now().UTC(), time.Now); err != nil && ctx.Err() == nil {
			logger.Warn("airport weather forecast refresh unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (w *WeatherForecastCache) refresh(ctx context.Context, store *Store, now time.Time, clock func() time.Time) error {
	w.mu.Lock()
	for station, report := range w.byStation {
		report.periods = slices.DeleteFunc(report.periods, func(period feeds.TAFPeriod) bool { return !period.EndsAt.After(now) })
		if len(report.periods) == 0 {
			delete(w.byStation, station)
			continue
		}
		w.byStation[station] = report
	}
	w.mu.Unlock()

	stations, err := store.weatherStationBatch(ctx, now, weatherForecastLead, weatherForecastBatchSize, &w.after)
	if err != nil || len(stations) == 0 {
		return err
	}
	periods, err := w.provider.Forecasts(ctx, stations, now)
	if err != nil {
		return err
	}

	// Each successful station response replaces its previous periods. Clear
	// and cancelled reports therefore remove prior adverse forecasts as well.
	batch := make(map[string][]feeds.TAFPeriod, len(stations))
	for _, station := range stations {
		batch[station] = nil
	}
	for _, period := range periods {
		if _, requested := batch[period.Airport]; requested && validWeatherForecastPeriod(period, now) {
			batch[period.Airport] = append(batch[period.Airport], period)
		}
	}
	for station, stationPeriods := range batch {
		slices.SortFunc(stationPeriods, compareWeatherForecastPeriods)
		batch[station] = slices.CompactFunc(stationPeriods, func(a, b feeds.TAFPeriod) bool {
			return compareWeatherForecastPeriods(a, b) == 0
		})
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	availableAt := clock().UTC()
	if availableAt.Before(now) {
		availableAt = now
	}
	for station, stationPeriods := range batch {
		if len(stationPeriods) == 0 {
			delete(w.byStation, station)
			continue
		}
		w.byStation[station] = stationWeatherForecast{availableAt: availableAt, periods: stationPeriods}
	}
	return nil
}

func validWeatherForecastPeriod(period feeds.TAFPeriod, now time.Time) bool {
	return period.Conditions != "" && !period.IssuedAt.IsZero() && !period.IssuedAt.After(now) &&
		!period.StartsAt.IsZero() && period.StartsAt.Before(period.EndsAt) && period.EndsAt.After(now) &&
		period.Probability >= 0 && period.Probability <= 100 && weatherForecastQualifierOrder(period.Qualifier) < 4
}

func weatherForecastQualifierOrder(qualifier string) int {
	switch qualifier {
	case "prevailing":
		return 0
	case "becoming":
		return 1
	case "temporary":
		return 2
	case "probability":
		return 3
	default:
		return 4
	}
}

func compareWeatherForecastPeriods(a, b feeds.TAFPeriod) int {
	if order := cmp.Compare(weatherForecastQualifierOrder(a.Qualifier), weatherForecastQualifierOrder(b.Qualifier)); order != 0 {
		return order
	}
	if order := b.StartsAt.Compare(a.StartsAt); order != 0 {
		return order
	}
	if order := a.EndsAt.Compare(b.EndsAt); order != 0 {
		return order
	}
	if order := cmp.Compare(b.Probability, a.Probability); order != 0 {
		return order
	}
	if order := b.IssuedAt.Compare(a.IssuedAt); order != 0 {
		return order
	}
	return cmp.Compare(a.Conditions, b.Conditions)
}

func (w *WeatherForecastCache) ForFlight(flight feeds.Flight, now time.Time) []AirportWeatherForecast {
	if w == nil || finished(flight) || flight.Status == "diverted" {
		return nil
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	var forecasts []AirportWeatherForecast
	forWeatherOperations(flight, func(airport, operation string, expected *time.Time) {
		if expected == nil || expected.Before(now.Add(-weatherLookback)) || expected.After(now.Add(weatherForecastLead)) {
			return
		}
		at := *expected
		if at.Before(now) {
			at = now
		}
		report := w.byStation[airport]
		// An older flight observation cannot use a forecast accepted later,
		// even when its product was issued before that observation.
		if report.availableAt.After(now) {
			return
		}
		remaining := weatherForecastRowLimit
		for _, period := range report.periods {
			if period.IssuedAt.After(now) || period.StartsAt.After(at) || !period.EndsAt.After(at) {
				continue
			}
			forecasts = append(forecasts, AirportWeatherForecast{
				Airport: airport, Operation: operation, Conditions: period.Conditions, IssuedAt: period.IssuedAt,
				StartsAt: period.StartsAt, EndsAt: period.EndsAt, Qualifier: period.Qualifier, Probability: period.Probability,
			})
			remaining--
			if remaining == 0 {
				break
			}
		}
	})
	return forecasts
}
