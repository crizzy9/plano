package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestAeroInboundObservationRefreshesOnlyRotationAndForecast(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "inbound.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Add(-time.Second).Truncate(time.Second)
	departure := now.Add(24*time.Hour + 15*time.Minute)
	onward := feeds.Flight{ID: "onward-1", Number: "AA2", Source: "flightaware_aeroapi", Status: "scheduled", InboundID: "inbound-1",
		Origin: "JFK", OriginICAO: "KJFK", TailNumber: "N123AA", ScheduledDeparture: &departure, ObservedAt: now.Add(-50 * time.Second)}
	inbound := feeds.Flight{ID: onward.InboundID, Number: "AA1", Source: onward.Source, Status: "scheduled",
		Origin: "LAX", OriginICAO: "KLAX", Destination: "JFK", DestinationICAO: "KJFK", TailNumber: onward.TailNumber,
		ScheduledArrival: new(departure.Add(-50 * time.Minute)), ObservedAt: now.Add(-time.Minute)}
	earlier := feeds.Flight{ID: "earlier-1", Source: onward.Source, Destination: "LAX", DestinationICAO: "KLAX", TailNumber: onward.TailNumber}
	rotation := []feeds.Flight{inbound, earlier}
	if err := store.Observe(ctx, onward, nil, rotation, nil); err != nil {
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
	if err := store.Track(ctx, owner, onward.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, owner, Device{Platform: "ios", Kind: "alert", Token: "fixture-ios"}, now); err != nil {
		t.Fatal(err)
	}
	apply := func(f feeds.Flight) {
		t.Helper()
		if err := store.IngestAeroObservation(ctx, f, now); err != nil {
			t.Fatal(err)
		}
	}
	details := func() Details {
		t.Helper()
		d, err := store.Details(ctx, onward.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	assertPending := func(want int) {
		t.Helper()
		var pending int
		err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox o`+outboxFlightEvent+`
WHERE o.flight_id=? AND o.done=0 AND json_extract(e.data,'$.kind')='delay_prediction'`, onward.ID).Scan(&pending)
		if err != nil || pending != want {
			t.Fatalf("pending delay alerts = %d, want %d, error %v", pending, want, err)
		}
	}
	delayed := inbound
	delayed.ObservedAt, delayed.EstimatedArrival = now.Add(-30*time.Second), new(departure.Add(20*time.Minute))
	delayed.Origin, delayed.OriginICAO = "ORD", "KORD"
	apply(delayed)
	apply(delayed) // Repeated callbacks for one observation are idempotent.
	apply(inbound) // A later callback can still return older provider evidence.
	if _, err := store.Flight(ctx, inbound.ID); err != ErrNotFound {
		t.Fatalf("inbound-only observation entered the shared flight cache: %v", err)
	}
	got := details()
	if !got.Flight.ObservedAt.Equal(onward.ObservedAt) || got.Flight.EstimatedDeparture != nil ||
		got.Prediction == nil || got.Prediction.DelayMinutes != 60 || len(got.Inbound) != 1 || got.Inbound[0].Origin != "ORD" {
		t.Fatalf("onward callback state = %+v", got)
	}
	if len(got.Events) != 1 || got.Events[0].Kind != "delay_prediction" || !got.Events[0].At.Equal(now) {
		t.Fatalf("callback events = %+v", got.Events)
	}
	assertPending(1)
	// The poll's exact lookup can fail before the callback, even when the
	// subsequent tail lookup starts after the callback observation.
	onward.ObservedAt = now.Add(-40 * time.Second)
	inferred := inbound
	inferred.ID, inferred.ObservedAt = "inferred-1", now.Add(-20*time.Second)
	if err := store.observeTracked(ctx, onward, nil, []feeds.Flight{inferred}, nil); err != nil {
		t.Fatal(err)
	}
	if got := details(); len(got.Inbound) != 1 || got.Inbound[0].ID != inbound.ID || got.Prediction == nil || got.Prediction.DelayMinutes != 60 {
		t.Fatalf("late fallback lookup replaced the callback evidence: %+v", got)
	}
	// A poll that began before the callback can have a newer onward timestamp
	// and still carry the older inbound observation and aircraft chain.
	onward.ObservedAt = now
	if err := store.observeTracked(ctx, onward, nil, rotation, nil); err != nil {
		t.Fatal(err)
	}
	got = details()
	if len(got.Inbound) != 1 || !got.Inbound[0].ObservedAt.Equal(delayed.ObservedAt) || got.Prediction == nil || got.Prediction.DelayMinutes != 60 {
		t.Fatalf("racing poll replaced the callback evidence: %+v", got)
	}
	diverted := delayed
	diverted.Status, diverted.Destination, diverted.DestinationICAO = "diverted", "BOS", "KBOS"
	diverted.ObservedAt = now.Add(time.Millisecond)
	if err := store.applyAeroInbound(ctx, inbound.ID, diverted, now.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	onward.ObservedAt = now.Add(2 * time.Nanosecond)
	if err := store.observeTracked(ctx, onward, nil, []feeds.Flight{}, nil); err != nil {
		t.Fatal(err)
	}
	got = details()
	if got.Prediction != nil || len(got.Inbound) != 1 || got.Inbound[0].Status != "diverted" {
		t.Fatalf("disrupted inbound lost to a stale empty poll: %+v", got)
	}
	onward.ObservedAt = now.Add(3 * time.Nanosecond)
	if err := store.observeTracked(ctx, onward, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	onward.ObservedAt = now.Add(4 * time.Nanosecond)
	if err := store.observeTracked(ctx, onward, nil, rotation, nil); err != nil {
		t.Fatal(err)
	}
	got = details()
	if got.Prediction != nil || len(got.Inbound) != 1 || got.Inbound[0].Status != "diverted" {
		t.Fatalf("disrupted inbound lost to an older poll: %+v", got)
	}
	assertPending(0)

	// The poll read its advisory snapshot before the callback but fetched
	// the onward leg after the inbound observation. Rotation then failed.
	prefetched := feeds.Advisory{ID: "wind", Source: feeds.FAASWIM, Airport: onward.OriginICAO, Kind: "departure_delay",
		Direction: "departure", Reason: "earlier wind", DelayMinutes: new(25), ObservedAt: now.Add(-time.Minute),
		EndsAt: new(departure.Add(time.Hour))}
	current := prefetched
	current.Reason, current.DelayMinutes, current.ObservedAt = "current wind", new(35), time.Now().UTC()
	if err := store.PublishAdvisory(ctx, current, current.ObservedAt); err != nil {
		t.Fatal(err)
	}
	onward.ObservedAt = now.Add(2 * time.Millisecond)
	if err := store.observeTracked(ctx, onward, nil, nil, []feeds.Advisory{prefetched}); err != nil {
		t.Fatal(err)
	}
	got = details()
	if got.Prediction == nil || got.Prediction.DelayMinutes != 35 || !strings.Contains(strings.Join(got.Prediction.Reasons, " "), current.Reason) {
		t.Fatalf("rotation outage reused prefetched advisory evidence: %+v", got.Prediction)
	}
}

func TestAeroInboundObservationReplacesInferredFallback(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "inbound-fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Add(-time.Second).Truncate(time.Second)
	departure := now.Add(time.Hour)
	onward := feeds.Flight{ID: "AAL33-onward", Number: "AA33", Source: "flightaware_aeroapi", Status: "scheduled",
		Origin: "JFK", OriginICAO: "KJFK", TailNumber: "N123AA", InboundID: "AAL32-linked",
		ScheduledDeparture: &departure, ObservedAt: now.Add(-2 * time.Minute)}
	fallback := feeds.Flight{ID: "AAL31-fallback", Number: "AA31", Source: onward.Source, Status: "scheduled",
		Origin: "LAX", OriginICAO: "KLAX", Destination: "JFK", DestinationICAO: "KJFK", TailNumber: onward.TailNumber,
		ScheduledDeparture: new(now.Add(-5 * time.Hour)), ScheduledArrival: new(departure.Add(-50 * time.Minute)),
		AircraftModel: "fallback model", AircraftAge: new(12), ObservedAt: now.Add(-time.Minute)}
	earlier := feeds.Flight{ID: "AAL30-earlier", Source: onward.Source, Destination: "LAX", DestinationICAO: "KLAX", TailNumber: onward.TailNumber}
	if err := store.Observe(ctx, onward, nil, []feeds.Flight{fallback, earlier}, nil); err != nil {
		t.Fatal(err)
	}
	trackAeroCallbackFixture(t, store, onward.ID)
	if err := store.QueueAeroRefresh(ctx, onward.InboundID, now); err != nil {
		t.Fatal(err)
	}
	if job, err := store.claimAeroRefresh(ctx, now); err != nil || job == nil || job.providerID != onward.InboundID {
		t.Fatalf("recovered inbound job = %+v, error %v", job, err)
	}
	linked := fallback
	linked.ID, linked.Number, linked.ObservedAt = onward.InboundID, "AA32", now
	linked.AircraftModel, linked.AircraftAge = "", nil
	linked.EstimatedArrival = new(departure.Add(20 * time.Minute))
	for _, tc := range []struct {
		name   string
		change func(*feeds.Flight)
	}{
		{"older observation", func(f *feeds.Flight) { f.ObservedAt = fallback.ObservedAt.Add(-time.Second) }},
		{"wrong route", func(f *feeds.Flight) { f.Destination, f.DestinationICAO = "BOS", "KBOS" }},
		{"wrong aircraft", func(f *feeds.Flight) { f.TailNumber = "N321AA" }},
		{"missing departure", func(f *feeds.Flight) { f.ScheduledDeparture = nil }},
		{"departure after onward", func(f *feeds.Flight) { f.ScheduledDeparture = new(departure.Add(time.Minute)) }},
		{"arrival outside rotation window", func(f *feeds.Flight) {
			f.ScheduledDeparture = new(departure.Add(-30 * time.Hour))
			f.ActualArrival = new(departure.Add(-26 * time.Hour))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := linked
			tc.change(&invalid)
			if err := store.IngestAeroObservation(t.Context(), invalid, now); err != nil {
				t.Fatal(err)
			}
			got, err := store.Details(t.Context(), onward.ID)
			if err != nil || len(got.Inbound) != 2 || got.Inbound[0].ID != fallback.ID || got.Inbound[1].ID != earlier.ID {
				t.Fatalf("invalid replacement altered cached rotation: %+v, error %v", got.Inbound, err)
			}
		})
	}
	if err := store.IngestAeroObservation(ctx, linked, now); err != nil {
		t.Fatal(err)
	}
	got, err := store.Details(ctx, onward.ID)
	if err != nil || !got.Flight.ObservedAt.Equal(onward.ObservedAt) || len(got.Inbound) != 1 ||
		got.Inbound[0].ID != linked.ID || got.Inbound[0].AircraftModel != "" || got.Inbound[0].AircraftAge != nil ||
		got.Prediction == nil || got.Prediction.DelayMinutes != 60 {
		t.Fatalf("recovered inbound state = %+v, error %v", got, err)
	}
}
