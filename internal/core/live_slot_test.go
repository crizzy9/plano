package core

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestLiveActivityUpdateRotationPreservesOtherFlightsAndSlots(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "slots.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	departure := now.Add(20 * time.Minute)
	arrival := now.Add(4 * time.Hour)
	for _, id := range []string{"flight-one", "flight-two"} {
		flight := feeds.Flight{ID: id, Number: "UA123", Status: "scheduled", Origin: "SFO", Destination: "JFK",
			ScheduledDeparture: &departure, ScheduledArrival: &arrival, ObservedAt: now}
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	owner, other := slotInstallation(t, store), slotInstallation(t, store)
	for _, owner := range []string{owner, other} {
		for _, id := range []string{"flight-one", "flight-two"} {
			if err := store.Track(ctx, owner, id, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	slotOne := "activity:" + strings.Repeat("a", 64)
	slotTwo := "activity:" + strings.Repeat("b", 64)
	slotThree := "activity:" + strings.Repeat("c", 64)
	update := func(token, flight, slot string) Device {
		d := Device{Platform: "ios", Kind: "live_update", Token: token, FlightID: flight, LiveSlot: slot}
		if slot != "" {
			d.LiveRevision = 1
		}
		return d
	}
	old := update("initial-update", "flight-one", slotOne)
	parallel := update("parallel-update", "flight-one", slotTwo)
	otherFlight := update("other-flight-update", "flight-two", slotThree)
	legacy := update("legacy-update", "flight-one", "")
	otherOwner := update("other-owner-update", "flight-one", slotOne)
	for _, d := range []Device{old, parallel, otherFlight, legacy} {
		if err := store.Register(ctx, owner, d, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Register(ctx, other, otherOwner, now); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.claim(ctx, now, 16)
	if err != nil || len(jobs) != 5 {
		t.Fatalf("claim before rotation: %d jobs, %v", len(jobs), err)
	}
	var obsolete deliveryJob
	for _, job := range jobs {
		if job.target.Token == old.Token {
			obsolete = job
		}
	}
	if obsolete.deviceID == 0 {
		t.Fatal("initial activity update was missing")
	}
	for _, d := range []Device{
		update("collision", "flight-two", slotOne),
		update(parallel.Token, "flight-one", slotOne),
		update(otherFlight.Token, "flight-one", slotOne),
	} {
		d.LiveRevision = 2
		if err := store.Register(ctx, owner, d, now.Add(time.Second)); !errors.Is(err, ErrInvalidLiveSlot) {
			t.Fatalf("collision returned %v", err)
		}
	}
	assertSlotDevice(t, store, old.Token, owner, slotOne)
	replacement := update("replacement-update", "flight-one", slotOne)
	replacement.LiveRevision = 2
	if err := store.Register(ctx, owner, replacement, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, owner, replacement, now); err != nil {
		t.Fatal(err)
	}
	assertSlotDevice(t, store, old.Token, "", "")
	assertSlotDevice(t, store, replacement.Token, owner, slotOne)
	assertSlotDevice(t, store, parallel.Token, owner, slotTwo)
	assertSlotDevice(t, store, otherFlight.Token, owner, slotThree)
	assertSlotDevice(t, store, otherOwner.Token, other, slotOne)
	assertSlotDevice(t, store, legacy.Token, owner, "")
	var obsoleteJobs int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE device_id=?`, obsolete.deviceID).Scan(&obsoleteJobs); err != nil || obsoleteJobs != 0 {
		t.Fatalf("old token has %d jobs, %v", obsoleteJobs, err)
	}
	if err := store.resolve(ctx, obsolete, &push.DeliveryError{InvalidToken: true}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	jobs, err = store.claim(ctx, now.Add(2*time.Second), 16)
	if err != nil || len(jobs) != 1 || jobs[0].target.Token != replacement.Token || jobs[0].message.Kind != push.LiveUpdate {
		t.Fatalf("rotation did not enqueue one replacement update: %d jobs, %v", len(jobs), err)
	}
	if !jobs[0].message.Timestamp.Equal(time.Unix(obsolete.message.Timestamp.Unix()+1, 0)) {
		t.Fatal("same-second rotation lost the wire clock or its retry advanced the clock again")
	}
	// Legacy exact-token deletion may still be used after rotating a slot.
	old.LiveSlot, old.LiveRevision = "", 0
	replacement.LiveSlot, replacement.LiveRevision = "", 0
	if err := store.RemoveDevice(ctx, owner, old); err != nil {
		t.Fatal(err)
	}
	assertSlotDevice(t, store, replacement.Token, owner, slotOne)
	if err := store.RemoveDevice(ctx, owner, replacement); err != nil {
		t.Fatal(err)
	}
	assertSlotDevice(t, store, replacement.Token, "", "")
	restored := update("restored-update", "flight-one", slotOne)
	restored.LiveRevision = 3
	if err := store.Register(ctx, owner, restored, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestLiveActivityStartSlotSupportsTokenOwnershipMigration(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "slots.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, other := slotInstallation(t, store), slotInstallation(t, store)
	now := time.Now().UTC()
	start := Device{Platform: "ios", Kind: "live_start", Token: "initial-start", LiveSlot: "start", LiveRevision: 1, RequestUpdateToken: true}
	replacement := Device{Platform: "ios", Kind: "live_start", Token: "replacement-start", LiveSlot: "start", LiveRevision: 2}
	legacy := Device{Platform: "ios", Kind: "live_start", Token: "legacy-start"}
	for _, d := range []Device{start, legacy, replacement} {
		if err := store.Register(ctx, owner, d, now); err != nil {
			t.Fatal(err)
		}
	}
	assertSlotDevice(t, store, start.Token, "", "")
	assertSlotDevice(t, store, replacement.Token, owner, "start")
	assertSlotDevice(t, store, legacy.Token, owner, "")
	var staleCapability int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM live_token_requests`).Scan(&staleCapability); err != nil || staleCapability != 0 {
		t.Fatalf("old start token capability persisted: %d, %v", staleCapability, err)
	}
	if err := store.Register(ctx, other, replacement, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	assertSlotDevice(t, store, replacement.Token, other, "start")
	start.LiveRevision = 3
	if err := store.Register(ctx, owner, start, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertSlotDevice(t, store, start.Token, owner, "start")
	assertSlotDevice(t, store, replacement.Token, other, "start")
}

func TestLiveActivityStartRotationKeepsOnlyConfirmedStarts(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "slots.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := slotInstallation(t, store)
	now := time.Now().UTC().Truncate(time.Second)
	departure := now.Add(30 * time.Minute)
	old := Device{Platform: "ios", Kind: "live_start", Token: "initial-start", LiveSlot: "start", LiveRevision: 1}
	if err := store.Register(ctx, owner, old, now); err != nil {
		t.Fatal(err)
	}
	observe := func(flight feeds.Flight, at time.Time) {
		t.Helper()
		flight.ObservedAt = at
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	confirmed := feeds.Flight{ID: "confirmed", Number: "UA123", Origin: "SFO", Destination: "JFK", Status: "scheduled", ScheduledDeparture: &departure}
	pending, failed := confirmed, confirmed
	pending.ID, failed.ID = "pending", "failed"
	for _, flight := range []feeds.Flight{confirmed, pending, failed} {
		observe(flight, now)
		if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	observe(confirmed, now.Add(time.Second))
	jobs, err := store.claim(ctx, now.Add(2*time.Second), 10)
	if err != nil || len(jobs) != 1 || jobs[0].message.FlightID != confirmed.ID {
		t.Fatalf("unexpected first start batch: %d jobs, %v", len(jobs), err)
	}
	if err := store.resolve(ctx, jobs[0], nil, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	later := now.Add(11 * time.Minute)
	if err := store.Prune(ctx, later); err != nil {
		t.Fatal(err)
	}
	observe(failed, later)
	jobs, err = store.claim(ctx, later, 10)
	if err != nil || len(jobs) != 1 || jobs[0].message.FlightID != failed.ID {
		t.Fatalf("unexpected failed start batch: %d jobs, %v", len(jobs), err)
	}
	if err := store.resolve(ctx, jobs[0], errors.New("provider rejection"), later); err != nil {
		t.Fatal(err)
	}
	observe(pending, later)
	replacement := old
	replacement.Token = "replacement-start"
	replacement.LiveRevision = 2
	if err := store.Register(ctx, owner, replacement, later.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, flight := range []feeds.Flight{confirmed, pending, failed} {
		observe(flight, later.Add(2*time.Second))
	}
	jobs, err = store.claim(ctx, later.Add(3*time.Second), 10)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("rotation should requeue only pending and failed starts: %d jobs, %v", len(jobs), err)
	}
	seen := map[string]bool{}
	for _, job := range jobs {
		if job.target.Token != replacement.Token || job.message.Kind != push.LiveStart {
			t.Fatal("rotation queued a job for an obsolete token or incorrect kind")
		}
		seen[job.message.FlightID] = true
	}
	if !seen[pending.ID] || !seen[failed.ID] {
		t.Fatal("rotation duplicated a confirmed start or lost an unconfirmed one")
	}
}

func TestLiveActivitySlotRejectsOutOfOrderWritesAfterTokenRetirement(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "slots.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := slotInstallation(t, store)
	now := time.Now().UTC()
	old := Device{Platform: "ios", Kind: "live_start", Token: "older-token", LiveSlot: "start", LiveRevision: 1}
	current := Device{Platform: "ios", Kind: "live_start", Token: "current-token", LiveSlot: "start", LiveRevision: 2}
	for _, d := range []Device{current, old, current} {
		if err := store.Register(ctx, owner, d, now); err != nil {
			t.Fatal(err)
		}
	}
	assertSlotDevice(t, store, old.Token, "", "")
	assertSlotDevice(t, store, current.Token, owner, "start")
	conflicting := old
	conflicting.LiveRevision = current.LiveRevision
	if err := store.Register(ctx, owner, conflicting, now); !errors.Is(err, ErrInvalidLiveSlot) {
		t.Fatalf("same-revision token collision returned %v", err)
	}
	deleteCurrent := current
	deleteCurrent.LiveSlot, deleteCurrent.LiveRevision = "", 0
	if err := store.RemoveDevice(ctx, owner, deleteCurrent); err != nil {
		t.Fatal(err)
	}
	for _, d := range []Device{old, current} {
		if err := store.Register(ctx, owner, d, now); err != nil {
			t.Fatal(err)
		}
		assertSlotDevice(t, store, d.Token, "", "")
	}
	current.LiveRevision++
	if err := store.Register(ctx, owner, current, now); err != nil {
		t.Fatal(err)
	}
	assertSlotDevice(t, store, current.Token, owner, "start")
}

func slotInstallation(t *testing.T, store *Store) string {
	t.Helper()
	token, err := store.CreateInstallation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func assertSlotDevice(t *testing.T, store *Store, token, owner, slot string) {
	t.Helper()
	var gotOwner, gotSlot string
	err := store.db.QueryRowContext(t.Context(), `SELECT d.owner,COALESCE(l.slot,'') FROM devices d
LEFT JOIN device_live_slots l ON l.device_id=d.id WHERE d.token=?`, token).Scan(&gotOwner, &gotSlot)
	if owner == "" {
		if err == nil {
			t.Fatal("retired activity token is still present")
		}
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
	}
	if err != nil || gotOwner != owner || gotSlot != slot {
		t.Fatalf("unexpected device owner or slot: owner matches %v, slot matches %v, %v", gotOwner == owner, gotSlot == slot, err)
	}
}
