package core

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestExpiredClosurePredictionIsHiddenAndNotPreservedDuringOutage(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "forecast.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	now := time.Now().UTC()
	observed := now.Add(-45 * time.Minute)
	flight := feeds.Flight{ID: "closure-forecast", Number: "UA123", Origin: "SFO", OriginICAO: "KSFO", Destination: "EWR",
		Status: "scheduled", ScheduledDeparture: new(now.Add(-40 * time.Minute)), ObservedAt: observed}
	closure := feeds.Advisory{ID: "full-closure", Source: feeds.FAASWIM, Kind: "airport_closure", Airport: "KSFO",
		RawReason: "!SFO SFO AD AP CLSD", StartsAt: new(observed.Add(-time.Minute)), EndsAt: new(now.Add(time.Hour)), ObservedAt: observed}
	if err := store.Observe(ctx, flight, nil, nil, []feeds.Advisory{closure}); err != nil {
		t.Fatal(err)
	}
	readPrediction := func() *Prediction {
		t.Helper()
		var raw []byte
		if err := store.db.QueryRowContext(ctx, `SELECT prediction FROM flights WHERE id=?`, flight.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var prediction *Prediction
		if err := json.Unmarshal(raw, &prediction); err != nil {
			t.Fatal(err)
		}
		return prediction
	}
	if prior := readPrediction(); prior == nil || prior.ValidUntil == nil || !prior.ValidUntil.Before(now) {
		t.Fatalf("expected an expired persisted closure forecast: %+v", prior)
	}
	if details, err := store.Details(ctx, flight.ID); err != nil || details.Prediction != nil {
		t.Fatalf("expired forecast returned from details: %+v, %v", details.Prediction, err)
	}
	flight.ObservedAt = now
	if err := store.observeTracked(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if prior := readPrediction(); prior != nil {
		t.Fatalf("expired closure forecast survived missing advisories: %+v", prior)
	}
}

func TestRotationOutageExpiresElapsedETAAndRecoveryRebuildsForecast(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "elapsed-eta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	now := time.Now().UTC().Add(-time.Minute)
	observed := now.Add(-12 * time.Minute)
	departure, eta := now.Add(-5*time.Minute), now.Add(-2*time.Minute)
	flight := feeds.Flight{ID: "onward-eta", Number: "UA2", Origin: "SFO", InboundID: "inbound-eta",
		Status: "scheduled", ObservedAt: observed, ScheduledDeparture: &departure}
	leg := feeds.Flight{ID: flight.InboundID, Number: "UA1", Status: "airborne", Destination: "SFO",
		ObservedAt: observed, EstimatedArrival: &eta}
	if err := store.Observe(ctx, flight, nil, []feeds.Flight{leg}, nil); err != nil {
		t.Fatal(err)
	}
	readSaved := func() *Prediction {
		t.Helper()
		var raw []byte
		if err := store.db.QueryRowContext(ctx, `SELECT prediction FROM flights WHERE id=?`, flight.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var forecast *Prediction
		if err := json.Unmarshal(raw, &forecast); err != nil {
			t.Fatal(err)
		}
		return forecast
	}
	if original := readSaved(); original == nil || original.ValidUntil == nil || !original.ValidUntil.Equal(eta) {
		t.Fatalf("original forecast did not expire at the gate ETA: %+v", original)
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
	if details, err := store.Details(ctx, flight.ID); err != nil || details.Prediction != nil || len(details.Inbound) != 1 {
		t.Fatalf("elapsed ETA remained visible or history was lost: %+v, %v", details, err)
	}
	flight.ObservedAt = now
	source := &rotationRefreshFixture{flight: flight, err: feeds.ErrUnavailable}
	refresher := Refresher{Store: store, Flights: source, Logger: slog.New(slog.DiscardHandler)}
	if err := refresher.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if saved := readSaved(); saved != nil {
		t.Fatalf("failed refresh kept elapsed ETA prediction: %+v", saved)
	}
	leg.ObservedAt = now.Add(time.Second)
	leg.EstimatedArrival = new(now.Add(5 * time.Minute))
	source.flight.ObservedAt, source.legs, source.err = leg.ObservedAt, []feeds.Flight{leg}, nil
	if err := refresher.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	details, err := store.Details(ctx, flight.ID)
	if err != nil || details.Prediction == nil || details.Prediction.DelayMinutes != 50 || details.Prediction.ValidUntil == nil ||
		!details.Prediction.ValidUntil.Equal(*leg.EstimatedArrival) {
		t.Fatalf("fresh ETA did not rebuild forecast: %+v, %v", details.Prediction, err)
	}
	// The next poll crosses the ETA without an intervening expiry refresh.
	leg.ObservedAt = *leg.EstimatedArrival
	leg.EstimatedArrival = new(leg.ObservedAt.Add(2 * time.Minute))
	source.flight.ObservedAt, source.legs = leg.ObservedAt, []feeds.Flight{leg}
	if err := refresher.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	assertFlightAlertHistory(t, store, flight.ID, "check_in|Check check-in availability for UA2 with your airline.",
		"delay_prediction|UA2 · 50 min later", "delay_prediction|UA2 · 52 min later")
}

func TestPublishedScheduleChangeRetiresSavedPredictionAndAlerts(t *testing.T) {
	store, flight := flightAlertDispatchFixture(t)
	flight.InboundID = "schedule-direct"
	flight.ScheduledDeparture = new(flight.ObservedAt.Add(30 * time.Hour))
	leg := feeds.Flight{ID: flight.InboundID, Number: "AA1", Destination: flight.Origin, Status: "airborne",
		ObservedAt: flight.ObservedAt, EstimatedArrival: new(flight.ScheduledDeparture.Add(10 * time.Minute))}
	flight.ObservedAt = flight.ObservedAt.Add(time.Millisecond)
	if err := store.observeTracked(t.Context(), flight, nil, []feeds.Flight{leg}, nil); err != nil {
		t.Fatal(err)
	}
	oldJobs := claimFlightAlertJobs(t, store, 2)
	flight.ScheduledDeparture = new(flight.ScheduledDeparture.Add(25 * time.Minute))
	observeFlightAlertFixture(t, store, &flight)
	if details, err := store.Details(t.Context(), flight.ID); err != nil || details.Prediction != nil {
		t.Fatalf("forecast kept the previous schedule baseline: %+v, %v", details.Prediction, err)
	}
	assertFlightAlertSends(t, store, oldJobs)
	flight.ObservedAt = flight.ObservedAt.Add(time.Millisecond)
	if err := store.observeTracked(t.Context(), flight, nil, []feeds.Flight{leg}, nil); err != nil {
		t.Fatal(err)
	}
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2), "Departure delay predicted|AA2 · 25 min later")
}
