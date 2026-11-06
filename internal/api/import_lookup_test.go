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

func TestLookupLocalDepartureMatchesOnlyExactAirportTime(t *testing.T) {
	const day = "2026-12-07"
	departure := time.Date(2026, time.December, 8, 6, 0, 29, 0, time.UTC)
	flight := func(id, zone string, at time.Time) feeds.Flight {
		return feeds.Flight{ID: id, Number: "UA123", OriginTimeZone: zone, ScheduledDeparture: &at, ObservedAt: departure}
	}
	service := newImportTestServer(t)
	var dates []string
	service.Source = tripItTestFlightSource{search: func(_ context.Context, _ string, date time.Time) ([]feeds.Flight, error) {
		dates = append(dates, date.Format(time.DateOnly))
		return []feeds.Flight{
			flight("match", "America/Los_Angeles", departure),
			flight("match", "America/Los_Angeles", departure),
			flight("later", "America/Los_Angeles", departure.Add(time.Minute)),
			flight("next-day", "America/Los_Angeles", departure.AddDate(0, 0, 1)),
			flight("wrong-zone", "America/New_York", departure),
			flight("unknown-zone", "", departure),
			flight("host-zone", "Local", departure),
		}, nil
	}}
	matches, err := service.lookupLeg(t.Context(), itinerary.Leg{Number: "UA123", Date: day, LocalDeparture: "22:00"})
	if err != nil || !slices.Equal(dates, []string{day}) || len(matches) != 1 || matches[0].ID != "match" {
		t.Fatalf("lookups = %v, matches = %v, error = %v", dates, matches, err)
	}
}

func TestResolveLocalDepartureLeavesDSTFoldAmbiguous(t *testing.T) {
	service := newImportTestServer(t)
	earlier := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)
	service.Source = tripItTestFlightSource{search: func(context.Context, string, time.Time) ([]feeds.Flight, error) {
		return []feeds.Flight{
			{ID: "earlier", Number: "UA123", OriginTimeZone: "America/New_York", ScheduledDeparture: &earlier, ObservedAt: earlier},
			{ID: "later", Number: "UA123", OriginTimeZone: "America/New_York", ScheduledDeparture: &later, ObservedAt: earlier},
		}, nil
	}}
	parsed := itinerary.Itinerary{Legs: []itinerary.Leg{{Number: "UA123", Date: "2026-11-01", LocalDeparture: "01:30"}}}
	result, err := service.resolveItinerary(t.Context(), parsed)
	if err != nil || len(result.Flights) != 0 || !slices.Equal(result.Unmatched, []string{"UA123 · 2026-11-01"}) {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}
