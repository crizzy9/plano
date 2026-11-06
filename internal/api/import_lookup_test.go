package api

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/itinerary"
)

func TestLookupTimedImportUsesDistinctDatesAndFlightIDs(t *testing.T) {
	const previous, utcDay, next = "2026-12-07", "2026-12-08", "2026-12-09"
	departure := time.Date(2026, time.December, 8, 2, 0, 0, 0, time.UTC)
	flight := func(id string, offset time.Duration) feeds.Flight {
		at := departure.Add(offset)
		return feeds.Flight{ID: id, Number: "UA123", ScheduledDeparture: &at, ObservedAt: departure}
	}
	match := flight("match", 45*time.Minute)
	outOfRange := flight("out-of-range", 45*time.Minute+time.Second)
	other := flight("other", -45*time.Minute)
	exact := flight("match", 0)
	otherExact := flight("other", 0)
	for _, test := range []struct {
		name      string
		date      string
		flights   map[string][]feeds.Flight
		wantDates []string
		wantIDs   []string
		wantExact bool
	}{
		{
			name: "exact matches on source date take priority and remain ambiguous",
			date: previous,
			flights: map[string][]feeds.Flight{
				previous: {exact, exact, otherExact, outOfRange}, utcDay: {flight("alternate", 0)},
			},
			wantDates: []string{previous}, wantIDs: []string{"match", "other"}, wantExact: true,
		},
		{
			name: "exact fallback overrides nearby matches including the same instance",
			date: previous,
			flights: map[string][]feeds.Flight{
				previous: {match, other}, utcDay: {exact}, next: {exact},
			},
			wantDates: []string{previous, utcDay, next}, wantIDs: []string{"match"}, wantExact: true,
		},
		{
			name: "distinct exact matches on fallback dates remain ambiguous",
			date: previous,
			flights: map[string][]feeds.Flight{
				previous: {match}, utcDay: {exact}, next: {otherExact},
			},
			wantDates: []string{previous, utcDay, next}, wantIDs: []string{"match", "other"}, wantExact: true,
		},
		{
			name: "UTC date rollover deduplicates repeated instance",
			date: previous,
			flights: map[string][]feeds.Flight{
				previous: {outOfRange}, utcDay: {match}, next: {match},
			},
			wantDates: []string{previous, utcDay, next}, wantIDs: []string{"match"},
		},
		{
			name: "different nearby instances across source and fallback dates remain ambiguous",
			date: next,
			flights: map[string][]feeds.Flight{
				next: {match}, utcDay: {match}, previous: {other},
			},
			wantDates: []string{next, utcDay, previous}, wantIDs: []string{"match", "other"},
		},
		{
			name:      "inconsistent source date stays within lookup budget",
			date:      "2026-12-01",
			flights:   map[string][]feeds.Flight{next: {match}},
			wantDates: []string{utcDay, previous, next}, wantIDs: []string{"match"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newImportTestServer(t)
			var dates []string
			service.Source = tripItTestFlightSource{search: func(_ context.Context, _ string, date time.Time) ([]feeds.Flight, error) {
				day := date.Format(time.DateOnly)
				dates = append(dates, day)
				return test.flights[day], nil
			}}
			leg := itinerary.Leg{Number: "UA123", Date: test.date, UTCDeparture: departure}
			matches, err := service.lookupLeg(t.Context(), leg)
			if err != nil || !slices.Equal(dates, test.wantDates) {
				t.Fatalf("lookups = %v, error = %v, want %v", dates, err, test.wantDates)
			}
			var ids []string
			for _, match := range matches {
				ids = append(ids, match.ID)
				if test.wantExact && (match.ScheduledDeparture == nil || !match.ScheduledDeparture.Equal(departure)) {
					t.Fatalf("flight %s departure = %v, want %v", match.ID, match.ScheduledDeparture, departure)
				}
			}
			if !slices.Equal(ids, test.wantIDs) {
				t.Fatalf("matches = %v, want %v", ids, test.wantIDs)
			}
		})
	}
}

func TestLookupTimedImportStopsOnCancellationBetweenDates(t *testing.T) {
	service := newImportTestServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var dates []string
	service.Source = tripItTestFlightSource{search: func(_ context.Context, _ string, date time.Time) ([]feeds.Flight, error) {
		dates = append(dates, date.Format(time.DateOnly))
		cancel()
		return nil, nil
	}}
	leg := itinerary.Leg{Number: "UA123", Date: "2026-12-07", UTCDeparture: time.Date(2026, time.December, 8, 2, 0, 0, 0, time.UTC)}
	matches, err := service.lookupLeg(ctx, leg)
	if !errors.Is(err, context.Canceled) || len(matches) != 0 || !slices.Equal(dates, []string{leg.Date}) {
		t.Fatalf("lookups = %v, matches = %v, error = %v", dates, matches, err)
	}
}
