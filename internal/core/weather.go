package core

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const (
	weatherCadence  = 5 * time.Minute
	weatherLookback = time.Hour
	weatherLead     = 2 * time.Hour
)

type METARProvider interface {
	Observations(context.Context, []string, time.Time) ([]feeds.Advisory, error)
}

type AirportWeather struct {
	Airport    string    `json:"airport"`
	Operation  string    `json:"operation"`
	Conditions string    `json:"conditions"`
	ObservedAt time.Time `json:"observedAt"`
	ValidUntil time.Time `json:"validUntil"`
}

// WeatherCache holds only public, time-bounded station reports. A separate
// worker keeps weather requests off flight refresh and callback paths.
type WeatherCache struct {
	provider  METARProvider
	mu        sync.RWMutex
	byStation map[string]feeds.Advisory
	after     string
}

func NewWeatherCache(provider METARProvider) *WeatherCache {
	return &WeatherCache{provider: provider, byStation: make(map[string]feeds.Advisory)}
}

func (w *WeatherCache) Run(ctx context.Context, store *Store, logger *slog.Logger) {
	tick := time.NewTicker(weatherCadence)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := w.refresh(ctx, store, time.Now().UTC()); err != nil && ctx.Err() == nil {
			logger.Warn("airport weather observation refresh unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (w *WeatherCache) refresh(ctx context.Context, store *Store, now time.Time) error {
	w.mu.Lock()
	for station, observation := range w.byStation {
		if !observation.EndsAt.After(now) {
			delete(w.byStation, station)
		}
	}
	w.mu.Unlock()

	stations, err := store.weatherStationBatch(ctx, now, weatherLead, feeds.MaxMETARStations, &w.after)
	if err != nil {
		return err
	}
	if len(stations) == 0 {
		return nil
	}
	observations, err := w.provider.Observations(ctx, stations, now)
	if err != nil {
		return err
	}

	// A successful response replaces the batch, including stations that are
	// now clear. An outage retains only the previous report's remaining life.
	w.mu.Lock()
	defer w.mu.Unlock()
	requested := make(map[string]bool, len(stations))
	for _, station := range stations {
		requested[station] = true
		delete(w.byStation, station)
	}
	for _, observation := range observations {
		if requested[observation.Airport] && observation.EndsAt != nil && observation.EndsAt.After(now) && !observation.ObservedAt.After(now) {
			w.byStation[observation.Airport] = observation
		}
	}
	return nil
}

func (s *Store) weatherStationBatch(ctx context.Context, now time.Time, lead time.Duration, limit int, after *string) ([]string, error) {
	stations, err := s.weatherStations(ctx, now, lead, limit, *after)
	if err == nil && len(stations) == 0 && *after != "" {
		stations, err = s.weatherStations(ctx, now, lead, limit, "")
	}
	if err != nil {
		return nil, err
	}
	*after = ""
	if len(stations) == limit {
		*after = stations[len(stations)-1]
	}
	return stations, nil
}

func (s *Store) weatherStations(ctx context.Context, now time.Time, lead time.Duration, limit int, after string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `WITH tracked_snapshot AS NOT MATERIALIZED (
  SELECT snapshot FROM (SELECT DISTINCT flight_id FROM tracked) tracked_flights JOIN flights ON flights.id=tracked_flights.flight_id
  WHERE json_extract(snapshot,'$.status') NOT IN ('cancelled','arrived','diverted')
), operations AS (
  SELECT json_extract(snapshot,'$.originICAO') AS airport,
    COALESCE(json_extract(snapshot,'$.estimatedDeparture'),json_extract(snapshot,'$.scheduledDeparture')) AS operation_at
  FROM tracked_snapshot WHERE json_extract(snapshot,'$.actualDeparture') IS NULL AND json_extract(snapshot,'$.actualTakeoff') IS NULL
    AND json_extract(snapshot,'$.status') NOT IN ('taxi_out','airborne','landed')
  UNION ALL
  SELECT json_extract(snapshot,'$.destinationICAO'),
    COALESCE(json_extract(snapshot,'$.estimatedArrival'),json_extract(snapshot,'$.scheduledArrival'))
  FROM tracked_snapshot WHERE json_extract(snapshot,'$.actualArrival') IS NULL
)
SELECT DISTINCT airport FROM operations WHERE airport GLOB '[A-Z][A-Z][A-Z][A-Z]' AND airport>?
AND julianday(operation_at) BETWEEN julianday(?) AND julianday(?) ORDER BY airport LIMIT ?`,
		after, now.Add(-weatherLookback).Format(time.RFC3339), now.Add(lead).Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stations := make([]string, 0, limit)
	for rows.Next() {
		var station string
		if err := rows.Scan(&station); err != nil {
			return nil, err
		}
		stations = append(stations, station)
	}
	return stations, rows.Err()
}

func (w *WeatherCache) ForFlight(flight feeds.Flight, now time.Time) []AirportWeather {
	if w == nil || finished(flight) || flight.Status == "diverted" {
		return nil
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	var weather []AirportWeather
	add := func(airport, operation string, at *time.Time) {
		if at == nil || at.Before(now.Add(-weatherLookback)) || at.After(now.Add(weatherLead)) {
			return
		}
		observation, ok := w.byStation[airport]
		if !ok || observation.ObservedAt.After(now) || !observation.EndsAt.After(now) {
			return
		}
		weather = append(weather, AirportWeather{Airport: airport, Operation: operation, Conditions: observation.Reason, ObservedAt: observation.ObservedAt, ValidUntil: *observation.EndsAt})
	}
	forWeatherOperations(flight, add)
	return weather
}

func forWeatherOperations(flight feeds.Flight, add func(airport, operation string, at *time.Time)) {
	if flightBeforeDeparture(flight) {
		add(flight.OriginICAO, "departure", effectiveDeparture(flight))
	}
	if flight.ActualArrival == nil {
		add(flight.DestinationICAO, "arrival", effectiveArrival(flight))
	}
}
