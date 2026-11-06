package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestFlightLookupRecalculatesGroundStopForecastAfterEstimateChanges(t *testing.T) {
	ctx := t.Context()
	service := newImportTestServer(t)
	token, owner := newImportTestInstallation(t, service.Store)
	now := time.Now().UTC().Truncate(time.Second)
	departure := now.Add(time.Hour)
	flight := feeds.Flight{ID: "UAL88-lookup-forecast", Number: "UA88", Source: "flightaware_aeroapi", Status: "scheduled",
		Origin: "SFO", OriginICAO: "KSFO", Destination: "EWR", DestinationICAO: "KEWR",
		ScheduledDeparture: &departure, ObservedAt: now.Add(-2 * time.Minute)}
	firstStop := feeds.Advisory{ID: "first-stop", Source: feeds.FAASWIM, Airport: flight.DestinationICAO,
		Kind: "ground_stop", Direction: "arrival", Reason: "runway inspection", ObservedAt: now.Add(-5 * time.Minute),
		StartsAt: new(departure.Add(-10 * time.Minute)), EndsAt: new(departure.Add(30 * time.Minute))}
	secondStop := firstStop
	secondStop.ID, secondStop.Reason = "second-stop", "airport volume"
	secondStop.StartsAt, secondStop.EndsAt = new(departure.Add(45*time.Minute)), new(departure.Add(90*time.Minute))
	advisories := []feeds.Advisory{firstStop, secondStop}
	for _, advisory := range advisories {
		if err := service.Store.PublishAdvisory(ctx, advisory, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Store.Observe(ctx, flight, nil, nil, advisories); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	updated := flight
	service.Source = tripItTestFlightSource{search: func(_ context.Context, number string, _ time.Time) ([]feeds.Flight, error) {
		if number != flight.Number {
			t.Fatalf("unexpected provider search for %q", number)
		}
		return []feeds.Flight{updated}, nil
	}}
	handler := service.Handler()
	detail := func() core.Details {
		t.Helper()
		body := importTestRequest(t, handler, http.MethodGet, "/v1/flights/"+flight.ID, token, "", http.StatusOK)
		var got core.Details
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	lookup := func(estimate, observed time.Time) core.Details {
		t.Helper()
		updated.EstimatedDeparture, updated.ObservedAt = &estimate, observed
		path := "/v1/flights?number=" + flight.Number + "&date=" + departure.Format(time.DateOnly)
		importTestRequest(t, handler, http.MethodGet, path, token, "", http.StatusOK)
		got := detail()
		if got.Flight.EstimatedDeparture == nil || !got.Flight.EstimatedDeparture.Equal(estimate) {
			t.Fatalf("published estimate = %v, want %s", got.Flight.EstimatedDeparture, estimate)
		}
		return got
	}
	assertForecast := func(got *core.Prediction, want, superseded feeds.Advisory, minutes int) {
		t.Helper()
		if got == nil || got.Departure == nil || !got.Departure.Equal(*want.EndsAt) || got.DelayMinutes != minutes {
			t.Fatalf("prediction = %+v, want %d minutes ending at %s", got, minutes, want.EndsAt)
		}
		reasons := strings.Join(got.Reasons, " ")
		if !strings.Contains(reasons, want.Reason) || strings.Contains(reasons, superseded.Reason) {
			t.Fatalf("prediction reasons = %q, want %q without %q", reasons, want.Reason, superseded.Reason)
		}
	}
	assertForecast(detail().Prediction, firstStop, secondStop, 30)
	assertForecast(lookup(departure.Add(time.Hour), now.Add(-time.Minute)).Prediction, secondStop, firstStop, 90)
	if got := lookup(departure.Add(2*time.Hour), now).Prediction; got != nil {
		t.Fatalf("prediction still cites a ground stop after the estimate passed both windows: %+v", got)
	}
}
