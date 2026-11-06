package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/itinerary"
)

type calendarSyncFlight struct {
	Day     string `json:"day"`
	Code    string `json:"code"`
	UTCTime string `json:"utcTime,omitempty"`
}

func (s Server) syncCalendar(w http.ResponseWriter, r *http.Request, owner string) error {
	var input struct {
		Flights []calendarSyncFlight `json:"flights"`
	}
	if err := parse(w, r, &input, 2048); err != nil || len(input.Flights) == 0 || len(input.Flights) > itinerary.MaxFlights {
		return errInvalid
	}
	type candidate struct {
		leg       itinerary.Leg
		key       string
		expiresAt time.Time
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	firstDay, lastDay := today.AddDate(0, 0, -1), today.AddDate(0, 0, 32)
	candidates := make([]candidate, 0, len(input.Flights))
	seen := make(map[string]bool, len(input.Flights))
	for _, flight := range input.Flights {
		day, err := time.Parse(time.DateOnly, flight.Day)
		if err != nil || day.Before(firstDay) || day.After(lastDay) || !flightNumber.MatchString(flight.Code) {
			return errInvalid
		}
		leg := itinerary.Leg{Number: flight.Code, Date: flight.Day}
		key := flight.Day + " " + flight.Code
		if flight.UTCTime != "" {
			leg.UTCDeparture, err = time.Parse(time.DateOnly+"T150405", flight.Day+"T"+flight.UTCTime)
			if err != nil || len(flight.UTCTime) != 6 {
				return errInvalid
			}
			key += " " + flight.UTCTime
		}
		if !seen[key] {
			seen[key] = true
			candidates = append(candidates, candidate{leg: leg, key: key, expiresAt: day.AddDate(0, 0, 3)})
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	lookupCtx, cancelLookups := context.WithTimeout(ctx, 16*time.Second)
	defer cancelLookups()
	completed := make([]string, 0, len(candidates))
	matched := make([]core.CalendarImportLeg, 0, len(candidates))
	var lookupFailure error
	for _, candidate := range candidates {
		accepted, err := s.Store.CalendarImportCompleted(ctx, owner, candidate.key)
		if err != nil {
			return err
		}
		if !accepted {
			if err := lookupCtx.Err(); err != nil {
				lookupFailure = err
				continue
			}
			flights, err := s.lookupLeg(lookupCtx, candidate.leg)
			switch {
			case errors.Is(err, feeds.ErrNotFound), errors.Is(err, feeds.ErrInvalidQuery):
				continue
			case errors.Is(err, feeds.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
				lookupFailure = err
				continue
			case err != nil:
				return err
			}
			if len(flights) != 1 {
				continue
			}
			matched = append(matched, core.CalendarImportLeg{
				Key: candidate.key, FlightID: flights[0].ID, ExpiresAt: candidate.expiresAt,
			})
		}
		completed = append(completed, candidate.key)
	}
	if len(completed) == 0 && lookupFailure != nil {
		return lookupFailure
	}
	if len(matched) != 0 {
		if err := s.Store.CommitCalendarImport(ctx, owner, matched); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return feeds.ErrUnavailable
			}
			return err
		}
	}
	respond(w, http.StatusOK, struct {
		Completed []string `json:"completed"`
	}{Completed: completed})
	return nil
}
