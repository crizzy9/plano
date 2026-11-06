package core

import (
	"slices"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

type terminalWeatherEvidence struct {
	Forecasts []AirportWeatherForecast `json:"forecasts,omitempty"`
}

func isPrevailingTerminalWeather(period AirportWeatherForecast) bool {
	return period.Qualifier == "prevailing" && period.Probability == 0
}

func prevailingTerminalWeather(forecasts []AirportWeatherForecast) []AirportWeatherForecast {
	var prevailing []AirportWeatherForecast
	for _, period := range forecasts {
		if isPrevailingTerminalWeather(period) {
			prevailing = append(prevailing, period)
		}
	}
	return prevailing
}

func (s *Store) terminalWeatherForecastCurrent(forecast *Prediction, flight feeds.Flight, now time.Time) bool {
	if forecast == nil || forecast.TerminalWeatherEvidence == nil {
		return true
	}
	if s.weatherForecast == nil {
		return false
	}
	current := prevailingTerminalWeather(s.weatherForecast.ForFlight(flight, now))
	return slices.EqualFunc(forecast.TerminalWeatherEvidence.Forecasts, current, func(a, b AirportWeatherForecast) bool {
		return a.Airport == b.Airport && a.Operation == b.Operation && a.Conditions == b.Conditions &&
			a.IssuedAt.Equal(b.IssuedAt) && a.StartsAt.Equal(b.StartsAt) && a.EndsAt.Equal(b.EndsAt)
	})
}
