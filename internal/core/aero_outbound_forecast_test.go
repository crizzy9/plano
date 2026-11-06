package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestAeroOutboundObservationRefreshesForecastAndAlerts(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "outbound-forecast.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	scheduled := now.Add(2 * time.Hour)
	flight := feeds.Flight{
		ID: "outbound-forecast", Number: "AA2", Source: "flightaware_aeroapi", Status: "scheduled",
		Origin: "SFO", OriginICAO: "KSFO", TailNumber: "N123AA", InboundID: "inbound-forecast",
		ScheduledDeparture: &scheduled, ObservedAt: now.Add(-2 * time.Minute),
	}
	inbound := feeds.Flight{
		ID: flight.InboundID, Number: "AA1", Source: flight.Source, Status: "airborne", DestinationICAO: flight.OriginICAO,
		TailNumber: flight.TailNumber, EstimatedArrival: new(scheduled.Add(-20 * time.Minute)), ObservedAt: now.Add(-5 * time.Minute),
	}
	if err := store.Observe(ctx, flight, nil, []feeds.Flight{inbound}, nil); err != nil {
		t.Fatal(err)
	}
	owner := friendInstallation(t, store)
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, owner, Device{Platform: "ios", Kind: "alert", Token: "fixture-ios"}, now); err != nil {
		t.Fatal(err)
	}
	advisory := feeds.Advisory{ID: "wind", Source: feeds.FAASWIM, Airport: flight.OriginICAO,
		Kind: "departure_delay", Direction: "departure", Reason: "wind restrictions", DelayMinutes: new(25),
		ObservedAt: now.Add(-11 * time.Minute), EndsAt: new(scheduled.Add(2 * time.Hour))}
	polledAt := now.Add(-10 * time.Minute)
	if err := store.cachePolledFlightNotices(ctx, advisory.Source, []feeds.Advisory{advisory}, polledAt); err != nil {
		t.Fatal(err)
	}
	cacheDeadline := polledAt.Add(polledNoticeMaxAge)
	details := func() Details {
		t.Helper()
		got, err := store.Details(ctx, flight.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	assertPending := func(want int) {
		t.Helper()
		var count int
		var expires int64
		err := store.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(o.expires),0) FROM outbox o`+outboxFlightEvent+`
WHERE o.flight_id=? AND o.done=0 AND json_extract(e.data,'$.kind')='delay_prediction'`, flight.ID).Scan(&count, &expires)
		if err != nil || count != want || want > 0 && expires != cacheDeadline.UnixNano() {
			t.Fatalf("pending forecast jobs = %d, expiry = %s, error = %v, want %d jobs capped at %s", count, time.Unix(0, expires), err, want, cacheDeadline)
		}
	}
	apply := func(receivedAt time.Time) {
		t.Helper()
		if err := store.IngestAeroObservation(ctx, flight, receivedAt); err != nil {
			t.Fatal(err)
		}
	}
	if got := details(); got.Prediction == nil || got.Prediction.DelayMinutes != 20 {
		t.Fatalf("original forecast = %+v", got.Prediction)
	}
	flight.EstimatedDeparture = new(scheduled.Add(45 * time.Minute))
	flight.ObservedAt = now.Add(-10 * time.Second)
	apply(now)
	apply(now)
	flight.ObservedAt = now.Add(-5 * time.Second)
	apply(now)
	got := details()
	if !got.Flight.ObservedAt.Equal(flight.ObservedAt) || len(got.Inbound) != 1 || got.Inbound[0].ID != inbound.ID ||
		got.Prediction == nil || got.Prediction.DelayMinutes != 45 || got.Prediction.Departure == nil ||
		!got.Prediction.Departure.Equal(*flight.EstimatedDeparture) || got.Prediction.ValidUntil == nil ||
		!got.Prediction.ValidUntil.Equal(cacheDeadline) {
		t.Fatalf("callback forecast = %+v, rotation = %+v", got.Prediction, got.Inbound)
	}
	reasons := strings.Join(got.Prediction.Reasons, " ")
	if !strings.Contains(reasons, "Inbound flight AA1") || !strings.Contains(reasons, advisory.Reason) {
		t.Fatalf("callback forecast omitted current evidence: %s", reasons)
	}
	assertPending(1)
	flight.ObservedAt, flight.EstimatedDeparture = now, nil
	apply(now)
	if got := details(); got.Prediction == nil || got.Prediction.DelayMinutes != 25 {
		t.Fatalf("forecast after ETD withdrawal = %+v", got.Prediction)
	}
	assertPending(0)
	flight.ActualDeparture = new(scheduled.Add(25 * time.Minute))
	flight.ObservedAt, flight.Status = flight.ActualDeparture.Add(time.Second), "taxi_out"
	apply(flight.ObservedAt)
	got = details()
	if got.Prediction != nil {
		t.Fatalf("forecast survived gate-out: %+v", got.Prediction)
	}
	var alerts int
	for _, event := range got.Events {
		if event.Kind == "delay_prediction" {
			alerts++
		}
		if event.Kind == advisory.Kind {
			t.Fatalf("outbound callbacks replayed a cached advisory event: %+v", event)
		}
	}
	if alerts != 1 {
		t.Fatalf("forecast events = %d, want one", alerts)
	}
	assertPending(0)
}

func TestAeroOutboundObservationDropsForecastWithoutCurrentEvidence(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "outbound-forecast.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	flight := feeds.Flight{ID: "outbound-withdrawal", Source: "flightaware_aeroapi", Status: "scheduled", Origin: "SFO",
		ScheduledDeparture: new(now.Add(time.Hour)), ObservedAt: now.Add(-time.Minute)}
	advisory := feeds.Advisory{ID: "uncached", Source: feeds.FAASWIM, Airport: flight.Origin, Kind: "departure_delay",
		Direction: "departure", Reason: "wind restrictions", DelayMinutes: new(30), ObservedAt: flight.ObservedAt,
		EndsAt: new(now.Add(2 * time.Hour))}
	if err := store.Observe(ctx, flight, nil, nil, []feeds.Advisory{advisory}); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, friendInstallation(t, store), flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	flight.ObservedAt = now.Add(-time.Second)
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Details(ctx, flight.ID); err != nil || got.Prediction == nil {
		t.Fatalf("ordinary observation dropped the valid saved forecast: %+v, %v", got.Prediction, err)
	}
	flight.ObservedAt = now
	if err := store.IngestAeroObservation(ctx, flight, now); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Details(ctx, flight.ID); err != nil || got.Prediction != nil {
		t.Fatalf("authoritative callback kept forecast without current evidence: %+v, %v", got.Prediction, err)
	}
}
