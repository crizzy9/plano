package core

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func seedFutureCheckIn(t *testing.T, opens time.Time) (*Store, string, feeds.Flight, Device) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := friendInstallation(t, store)
	ctx := t.Context()
	registeredAt := opens.Add(-time.Hour)
	departure := opens.Add(24 * time.Hour)
	flight := feeds.Flight{ID: "check-in-scheduler", Number: "AA2", Origin: "JFK", Destination: "SFO", Status: "scheduled",
		ScheduledDeparture: &departure, ObservedAt: registeredAt}
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := trackFlight(ctx, tx, owner, flight.ID, "", registeredAt); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	device := Device{Platform: "android", Kind: "alert", Token: "scheduler-device"}
	if err := store.Register(ctx, owner, device, registeredAt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.outboxWake:
	default:
	}
	return store, owner, flight, device
}

func checkInSchedulerCounts(t *testing.T, store *Store, wantEvents, wantLatches, wantJobs int) {
	t.Helper()
	var events, latches, jobs int
	err := store.db.QueryRowContext(t.Context(), `SELECT
 (SELECT COUNT(*) FROM events WHERE json_extract(data,'$.kind')='check_in'),
 (SELECT COUNT(*) FROM check_in_queued),
 (SELECT COUNT(*) FROM outbox o`+outboxFlightEvent+` WHERE json_extract(e.data,'$.kind')='check_in')`).
		Scan(&events, &latches, &jobs)
	if err != nil || events != wantEvents || latches != wantLatches || jobs != wantJobs {
		t.Fatalf("check-in events/latches/jobs = %d/%d/%d, want %d/%d/%d, err = %v", events, latches, jobs, wantEvents, wantLatches, wantJobs, err)
	}
}

func TestCheckInSchedulerHonorsOpeningAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delay  time.Duration
		expiry time.Duration
	}{
		{name: "at opening", expiry: 2 * time.Hour},
		{name: "near departure", delay: 23 * time.Hour, expiry: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			opens := time.Now().UTC().Truncate(time.Millisecond).Add(time.Hour)
			store, _, _, _ := seedFutureCheckIn(t, opens)
			if err := store.queueDueCheckIns(ctx, opens.Add(-time.Nanosecond)); err != nil {
				t.Fatal(err)
			}
			checkInSchedulerCounts(t, store, 0, 0, 0)
			select {
			case <-store.outboxWake:
				t.Fatal("closed window woke the dispatcher")
			default:
			}
			due := opens.Add(tc.delay)
			for range 2 {
				if err := store.queueDueCheckIns(ctx, due); err != nil {
					t.Fatal(err)
				}
			}
			checkInSchedulerCounts(t, store, 1, 1, 1)
			jobs, err := store.claim(ctx, due, 2)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("claimed %d reminders, err = %v", len(jobs), err)
			}
			msg := jobs[0].message
			if msg.Title != "Check-in reminder" || !msg.Timestamp.Equal(due) || !msg.ExpiresAt.Equal(due.Add(tc.expiry)) {
				t.Fatalf("check-in message = %+v, want queue at %s and expiry %s", msg, due, due.Add(tc.expiry))
			}
		})
	}
}

func TestCheckInSchedulerRechecksCandidatesBeforeQueueing(t *testing.T) {
	for _, action := range []string{"untrack", "revoke", "cancel", "takeoff", "reschedule"} {
		t.Run(action, func(t *testing.T) {
			ctx := t.Context()
			opens := time.Now().UTC().Truncate(time.Millisecond).Add(time.Hour)
			store, owner, flight, device := seedFutureCheckIn(t, opens)
			candidates, err := store.dueCheckInFlights(ctx, opens)
			if err != nil || !slices.Equal(candidates, []string{flight.ID}) {
				t.Fatalf("candidates = %v, err = %v", candidates, err)
			}
			switch action {
			case "untrack":
				err = store.Untrack(ctx, owner, flight.ID)
			case "revoke":
				err = store.RemoveDevice(ctx, owner, device)
			default:
				flight.ObservedAt = opens.Add(-time.Minute)
				switch action {
				case "cancel":
					flight.Status = "cancelled"
				case "takeoff":
					flight.ActualTakeoff = &flight.ObservedAt
				case "reschedule":
					flight.EstimatedDeparture = new(flight.ScheduledDeparture.Add(time.Hour))
				}
				err = store.Observe(ctx, flight, nil, nil, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			candidates, err = store.dueCheckInFlights(ctx, opens)
			if err != nil || len(candidates) != 0 {
				t.Fatalf("obsolete candidates = %v, err = %v", candidates, err)
			}
			if err := store.queueScheduledCheckIn(ctx, flight.ID, opens); err != nil {
				t.Fatal(err)
			}
			checkInSchedulerCounts(t, store, 0, 0, 0)
		})
	}
}

func TestCheckInSchedulerReoffersOnlyUnclaimedRescheduledReminder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claimed bool
	}{
		{name: "unclaimed"},
		{name: "possibly submitted", claimed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			opens := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
			store, _, flight, _ := seedFutureCheckIn(t, opens)
			if err := store.queueDueCheckIns(ctx, opens); err != nil {
				t.Fatal(err)
			}
			if tc.claimed {
				if jobs, err := store.claim(ctx, opens, 1); err != nil || len(jobs) != 1 {
					t.Fatalf("claimed %d reminders, err = %v", len(jobs), err)
				}
			}
			// Both windows were closed when the provider captured this
			// snapshot. It reached the store after the first opening.
			flight.ObservedAt = opens.Add(-time.Second)
			flight.ScheduledDeparture = new(flight.ScheduledDeparture.Add(2 * time.Hour))
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			var latchAfterRevision int
			if tc.claimed {
				latchAfterRevision = 1
			}
			checkInSchedulerCounts(t, store, 1, latchAfterRevision, 0)
			if err := store.queueDueCheckIns(ctx, opens.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			wantJobs := 1 - latchAfterRevision
			checkInSchedulerCounts(t, store, 1, 1, wantJobs)
		})
	}
}
