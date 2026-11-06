package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestPredictionUsesOnlyTheVerifiedDirectInbound(t *testing.T) {
	now := time.Date(2026, time.November, 5, 19, 0, 0, 0, time.UTC)
	departure := now.Add(time.Hour)
	flight := feeds.Flight{ID: "out", InboundID: "direct", Origin: "SFO", OriginICAO: "KSFO", TailNumber: "N123AA", ScheduledDeparture: &departure}
	direct := feeds.Flight{ID: "direct", Number: "UA7", Destination: "SFO", DestinationICAO: "KSFO", TailNumber: "N123AA", Status: "airborne", ObservedAt: now,
		EstimatedArrival: new(departure.Add(10 * time.Minute))}
	revisit := direct
	revisit.ID, revisit.Number = "revisit", "UA5"
	revisit.ScheduledArrival = new(departure.Add(-5 * time.Minute))
	revisit.EstimatedArrival = new(departure.Add(80 * time.Minute))
	rotation := []feeds.Flight{direct, revisit}
	forecast := Predict(flight, rotation, nil, now)
	if forecast == nil || forecast.DelayMinutes != 50 || !forecast.Departure.Equal(departure.Add(50*time.Minute)) ||
		forecast.ValidUntil == nil || !forecast.ValidUntil.Equal(now.Add(inboundForecastTTL)) || !strings.Contains(forecast.Reasons[0], "UA7") {
		t.Fatalf("wrong direct inbound forecast: %+v", forecast)
	}
	for _, tc := range []struct {
		name   string
		change func(*feeds.Flight)
	}{
		{"known conflicting aircraft", func(leg *feeds.Flight) { leg.TailNumber = "N987AA" }},
		{"known conflicting ICAO", func(leg *feeds.Flight) { leg.DestinationICAO = "KOAK" }},
		{"provider snapshot from the future", func(leg *feeds.Flight) { leg.ObservedAt = now.Add(time.Second) }},
		{"expired gate estimate", func(leg *feeds.Flight) { leg.ObservedAt = now.Add(-inboundForecastTTL) }},
		{"diverted", func(leg *feeds.Flight) { leg.Status = "diverted" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := direct
			tc.change(&bad)
			if got := Predict(flight, []feeds.Flight{bad, revisit}, nil, now); got != nil {
				t.Fatalf("invalid first leg should not promote an earlier visit: %+v", got)
			}
		})
	}
}

func TestPredictionUsesRunwayArrivalAsAGateTimeFloor(t *testing.T) {
	now := time.Date(2026, time.November, 5, 19, 0, 0, 0, time.UTC)
	flight := feeds.Flight{ID: "out", Origin: "SFO", TailNumber: "N123AA", ScheduledDeparture: new(now.Add(20 * time.Minute))}
	leg := feeds.Flight{ID: "in", Number: "UA7", Destination: "SFO", TailNumber: "n123aa", Status: "landed", ObservedAt: now,
		ScheduledArrival: new(now.Add(-30 * time.Minute)), EstimatedArrival: new(now.Add(-4 * time.Minute)), ActualLanding: new(now.Add(-2 * time.Minute))}
	forecast := Predict(flight, []feeds.Flight{leg}, nil, now)
	if forecast == nil || forecast.DelayMinutes != 18 || forecast.ValidUntil == nil {
		t.Fatalf("observed wheels-on did not bound the gate estimate: %+v", forecast)
	}
	leg.ActualArrival = new(now.Add(-time.Minute))
	leg.ObservedAt = now
	later := now.Add(inboundForecastTTL + time.Minute)
	forecast = Predict(flight, []feeds.Flight{leg}, nil, later)
	if forecast == nil || forecast.DelayMinutes != 19 || forecast.ValidUntil != nil {
		t.Fatalf("recorded gate arrival was not retained as a fact: %+v", forecast)
	}
}

func TestRotationOutagePreservesHistoryButExpiresInboundPrediction(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "rotation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Add(-time.Minute)
	observed := now.Add(-45 * time.Minute)
	departure := now.Add(time.Hour)
	flight := feeds.Flight{ID: "out", InboundID: "direct", Origin: "SFO", ScheduledDeparture: &departure, ObservedAt: observed}
	leg := feeds.Flight{ID: "direct", Number: "UA7", Destination: "SFO", ObservedAt: observed, EstimatedArrival: new(departure.Add(10 * time.Minute))}
	if err := store.Observe(ctx, flight, nil, []feeds.Flight{leg, {ID: "earlier", ObservedAt: observed}}, nil); err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	details, err := store.Details(ctx, flight.ID)
	if err != nil || details.Prediction != nil || len(details.Inbound) != 2 {
		t.Fatalf("details during the outage: %+v %v", details, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE flights SET prediction=json_remove(prediction,'$.validUntil') WHERE id=?`, flight.ID); err != nil {
		t.Fatal(err)
	}
	if legacy, err := store.Details(ctx, flight.ID); err != nil || legacy.Prediction != nil {
		t.Fatalf("legacy expired rotation prediction remained visible: %+v %v", legacy.Prediction, err)
	}
	flight.ObservedAt = now
	source := &rotationRefreshFixture{flight: flight, legs: []feeds.Flight{}, err: feeds.ErrUnavailable}
	refresher := Refresher{Store: store, Flights: source, Logger: slog.New(slog.DiscardHandler)}
	if err := refresher.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	var storedPrediction string
	if err := store.db.QueryRowContext(ctx, `SELECT prediction FROM flights WHERE id=?`, flight.ID).Scan(&storedPrediction); err != nil || storedPrediction != "null" {
		t.Fatalf("expired estimate survived the failed refresh: %s %v", storedPrediction, err)
	}
	details, err = store.Details(ctx, flight.ID)
	if err != nil || len(details.Inbound) != 2 || details.Inbound[0].ID != leg.ID {
		t.Fatalf("failed refresh discarded history: %+v %v", details.Inbound, err)
	}
	leg.ObservedAt = now.Add(time.Second)
	source.flight.ObservedAt = leg.ObservedAt
	source.legs = []feeds.Flight{leg}
	if err := refresher.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	details, err = store.Details(ctx, flight.ID)
	if err != nil || len(details.Inbound) != 1 || details.Inbound[0].ID != leg.ID || details.Prediction == nil {
		t.Fatalf("verified prefix did not replace older rotation: %+v %v", details, err)
	}
	source.flight.InboundID, source.flight.ObservedAt = "replacement", now.Add(2*time.Second)
	source.legs = nil
	if err := refresher.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	details, err = store.Details(ctx, flight.ID)
	if err != nil || len(details.Inbound) != 0 {
		t.Fatalf("changed aircraft assignment retained obsolete history: %+v %v", details.Inbound, err)
	}
}

type rotationRefreshFixture struct {
	feeds.FlightSource
	flight feeds.Flight
	legs   []feeds.Flight
	err    error
}

func (s *rotationRefreshFixture) Get(context.Context, string) (feeds.Flight, error) {
	return s.flight, nil
}

func (s *rotationRefreshFixture) Rotation(context.Context, feeds.Flight) ([]feeds.Flight, error) {
	return s.legs, s.err
}
