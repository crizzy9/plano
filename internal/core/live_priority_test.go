package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestIOSLivePriorityPreservesPendingChangeAndRotation(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "live-priority.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	departure, arrival := now.Add(30*time.Minute), now.Add(4*time.Hour)
	flight := feeds.Flight{ID: "priority-flight", Number: "UA123", Origin: "SFO", Destination: "JFK",
		Status: "scheduled", ScheduledDeparture: &departure, ScheduledArrival: &arrival, DepartureGate: "A1"}
	observe := func(offset time.Duration) {
		t.Helper()
		flight.ObservedAt = now.Add(offset)
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	observe(0)
	owner := slotInstallation(t, store)
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	const binding = "0af06679-3c08-4f26-9a5b-15f31d4904ab"
	device := Device{Platform: "ios", Kind: "live_update", FlightID: flight.ID, Binding: binding,
		Token: "first-activity-token", LiveSlot: "activity:" + strings.Repeat("a", 64), LiveRevision: 1}
	register := func(d Device, offset time.Duration) {
		t.Helper()
		if err := store.Register(ctx, owner, d, now.Add(offset)); err != nil {
			t.Fatal(err)
		}
	}
	expectDelivered := func(offset time.Duration, token string, immediate bool) deliveryJob {
		t.Helper()
		jobs := liveJobsAt(t, store, now.Add(offset))
		if len(jobs) != 1 || jobs[0].target.Token != token || jobs[0].message.Kind != push.LiveUpdate ||
			jobs[0].message.APNsImmediateLiveUpdate != immediate {
			t.Fatalf("unexpected delivery at %s: %+v", offset, jobs)
		}
		return jobs[0]
	}
	register(device, 0)
	expectDelivered(0, device.Token, false)
	firstETA := arrival.Add(5 * time.Minute)
	flight.EstimatedArrival = &firstETA
	observe(5 * time.Second)
	expectDelivered(5*time.Second, device.Token, false)

	const gateOffset = 10 * time.Second
	flight.DepartureGate = "B7"
	observe(gateOffset)
	jobs, err := store.claimForPlatform(ctx, now.Add(gateOffset), 16, "ios")
	if err != nil || len(jobs) != 1 || !jobs[0].message.APNsImmediateLiveUpdate {
		t.Fatalf("gate change was not urgent: %+v, %v", jobs, err)
	}
	gateJob := jobs[0]
	const retryDelay = 2 * time.Minute
	if err := store.resolve(ctx, gateJob, &push.DeliveryError{Retryable: true, RetryAfter: retryDelay}, now.Add(gateOffset)); err != nil {
		t.Fatal(err)
	}
	observe(70 * time.Second) // A heartbeat must leave the original retry alone.
	secondOwner := slotInstallation(t, store)
	if err := store.Track(ctx, secondOwner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	second := device
	second.Token, second.LiveSlot, second.LiveRevision = "second-activity-token", "activity:"+strings.Repeat("b", 64), 2
	second.Binding = "7d2c159b-3166-4bd2-86c6-1da0332e57e1"
	if err := store.Register(ctx, secondOwner, second, now.Add(71*time.Second)); err != nil {
		t.Fatal(err)
	}
	expectDelivered(71*time.Second, second.Token, false)
	secondETA := firstETA.Add(5 * time.Minute)
	flight.EstimatedArrival = &secondETA
	observe(75 * time.Second)
	expectDelivered(75*time.Second, second.Token, false)
	if err := store.RemoveDevices(ctx, secondOwner); err != nil {
		t.Fatal(err)
	}
	retry := expectDelivered(gateOffset+retryDelay, device.Token, true)
	if retry.id != gateJob.id || retry.attempts != 1 || retry.message.State.DepartureGate != "B7" {
		t.Fatalf("routine refresh displaced the pending gate update: %+v", retry)
	}
	observe(131 * time.Second)
	expectDelivered(131*time.Second, device.Token, false)

	flight.Status = "boarding"
	observe(135 * time.Second)
	expectDelivered(135*time.Second, device.Token, true)
	flight.DepartureTerminal = "2"
	observe(140 * time.Second)
	replacement := device
	replacement.Token, replacement.LiveRevision = "rotated-activity-token", 3
	register(replacement, 141*time.Second)
	if job := expectDelivered(141*time.Second, replacement.Token, true); job.message.State.DepartureTerminal != "2" {
		t.Fatalf("rotation lost the terminal change: %+v", job.message.State)
	}
}

func TestDivertedLiveStateOmitsUnverifiedArrival(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	arrival := now.Add(40 * time.Minute)
	flight := feeds.Flight{ID: "diverted-flight", Number: "UA123", Destination: "JFK", Status: "diverted", ObservedAt: now,
		EstimatedArrival: &arrival, ArrivalGate: "B7", ArrivalTerminal: "4"}
	message := liveMessage(flight, push.LiveStart, "", now)
	state := message.State
	if state.Status != "diverted" || state.Destination != "JFK" ||
		state.ArrivalEpoch != 0 || state.ArrivalGate != "" || state.ArrivalTerminal != "" || message.Title != "UA123 · Flight diverted" {
		t.Fatalf("unexpected diverted live message: %+v, state: %+v", message, state)
	}
}
