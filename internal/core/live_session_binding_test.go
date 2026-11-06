package core

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/push"
)

func TestIOSActivityBindingSeparatesRestartsFromTokenRotation(t *testing.T) {
	store, owner, id := dispatcherFixture(t)
	ctx := t.Context()
	now := time.Now().UTC()
	flight, err := store.Flight(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	flight.ScheduledDeparture = new(now.Add(20 * time.Minute))
	if err := store.Track(ctx, owner, id, ""); err != nil {
		t.Fatal(err)
	}
	observe := func(at time.Time) {
		t.Helper()
		flight.ObservedAt = at
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	register := func(d Device) {
		t.Helper()
		if err := store.Register(ctx, owner, d, now); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(at time.Time) []deliveryJob {
		t.Helper()
		jobs, err := store.claim(ctx, at, 10)
		if err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	const oldBinding = "01234567-89ab-4cde-8123-456789abcdef"
	const newBinding = "abcdef01-2345-4678-9abc-def012345678"
	oldStart := Device{Platform: "ios", Kind: "live_start", Token: "first-start", LiveSlot: "start", LiveRevision: 1, Binding: oldBinding}
	register(oldStart)
	observe(now.Add(time.Second))
	jobs := claim(now.Add(2 * time.Second))
	if len(jobs) != 1 || jobs[0].message.Kind != push.LiveStart || jobs[0].target.Binding != oldBinding {
		t.Fatalf("initial bound start = %+v", jobs)
	}
	if err := store.resolve(ctx, jobs[0], nil, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	oldUpdate := Device{Platform: "ios", Kind: "live_update", Token: "first-update", FlightID: id,
		LiveSlot: "activity:" + strings.Repeat("a", 64), LiveRevision: 2, Binding: oldBinding}
	register(oldUpdate)
	jobs = claim(now.Add(3 * time.Second))
	if len(jobs) != 1 || jobs[0].message.Kind != push.LiveUpdate {
		t.Fatalf("initial bound update = %+v", jobs)
	}
	obsolete := jobs[0]

	newStart := oldStart
	newStart.Token, newStart.Binding, newStart.LiveRevision = "replacement-start", newBinding, 4
	register(newStart)
	legacy := Device{Platform: "ios", Kind: "live_start", Token: newStart.Token}
	register(legacy)
	if err := store.Register(ctx, slotInstallation(t, store), legacy, now); err != nil {
		t.Fatal(err)
	}
	assertSlotDevice(t, store, oldUpdate.Token, "", "")
	stale := oldUpdate
	stale.Token, stale.LiveSlot, stale.LiveRevision = "late-first-update", "activity:"+strings.Repeat("b", 64), 3
	register(stale)
	assertSlotDevice(t, store, stale.Token, "", "")
	conflict := newStart
	conflict.Binding = oldBinding
	if err := store.Register(ctx, owner, conflict, now); !errors.Is(err, ErrInvalidLiveSlot) {
		t.Fatalf("same revision changed ownership binding: %v", err)
	}
	observe(now.Add(4 * time.Second))
	jobs = claim(now.Add(5 * time.Second))
	if len(jobs) != 1 || jobs[0].message.Kind != push.LiveStart || jobs[0].target.Binding != newBinding {
		t.Fatalf("new session did not receive a fresh bound start: %+v", jobs)
	}
	logger := slog.New(slog.DiscardHandler)
	store.deliver(ctx, pushFunc(func(context.Context, push.Target, push.Message) error {
		t.Fatal("retired activity update reached transport")
		return nil
	}), logger, obsolete)
	if err := store.resolve(ctx, jobs[0], nil, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}

	newStart.Token, newStart.LiveRevision = "rotated-start", 5
	register(newStart)
	observe(now.Add(6 * time.Second))
	if jobs := claim(now.Add(7 * time.Second)); len(jobs) != 0 {
		t.Fatalf("same session token rotation queued %d duplicate starts", len(jobs))
	}
	if err := store.RemoveDevice(ctx, owner, Device{Platform: "ios", Kind: "live_start", Token: newStart.Token}); err != nil {
		t.Fatal(err)
	}
	assertSlotDevice(t, store, newStart.Token, owner, newStart.LiveSlot)
	var invalidated deliveryJob
	if err := store.db.QueryRowContext(ctx, `SELECT id,registered FROM devices WHERE owner=? AND token=?`, owner, newStart.Token).
		Scan(&invalidated.deviceID, &invalidated.registered); err != nil {
		t.Fatal(err)
	}
	if err := store.resolve(ctx, invalidated, &push.DeliveryError{InvalidToken: true}, now); err != nil {
		t.Fatal(err)
	}
	assertSlotDevice(t, store, newStart.Token, "", "")
	stale.Token, stale.LiveRevision = "late-update-after-start-invalidation", 5
	register(stale)
	assertSlotDevice(t, store, stale.Token, "", "")
}
