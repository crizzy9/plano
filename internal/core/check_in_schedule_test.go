package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/mattn/go-sqlite3"
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
			candidates, err := store.dueCheckInFlights(ctx, opens, nil)
			if err != nil || len(candidates) != 1 || candidates[0].id != flight.ID {
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
			candidates, err = store.dueCheckInFlights(ctx, opens, nil)
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

// Seed additional cached flights before their check-in windows open. Reuse
// the real observation/tracking paths without letting setup queue reminders.
func addFutureCheckIns(t *testing.T, store *Store, owner string, prototype feeds.Flight, prefix string, count int) []string {
	t.Helper()
	ctx := t.Context()
	ids := make([]string, 0, count)
	for i := range count {
		flight := prototype
		flight.ID = fmt.Sprintf("%s-%02d", prefix, i)
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := trackFlight(ctx, tx, owner, flight.ID, "", flight.ObservedAt); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, flight.ID)
	}
	select {
	case <-store.outboxWake:
	default:
	}
	return ids
}

func TestCheckInSchedulerDrainsCachedBurstInOnePass(t *testing.T) {
	opens := time.Date(2027, time.May, 18, 12, 0, 0, 0, time.UTC)
	store, owner, flight, device := seedFutureCheckIn(t, opens)
	const count = 3*checkInSchedulerBatch + 1
	wantIDs := append([]string{flight.ID}, addFutureCheckIns(t, store, owner, flight, "burst", count-1)...)
	checkInSchedulerCounts(t, store, 0, 0, 0)
	if err := store.queueDueCheckIns(t.Context(), opens); err != nil {
		t.Fatal(err)
	}
	checkInSchedulerCounts(t, store, count, count, count)
	select {
	case <-store.outboxWake:
	default:
		t.Fatal("queued burst did not wake the dispatcher")
	}
	// The first wake has been consumed, so an empty repeat must not wake.
	if err := store.queueDueCheckIns(t.Context(), opens); err != nil {
		t.Fatal(err)
	}
	checkInSchedulerCounts(t, store, count, count, count)
	select {
	case <-store.outboxWake:
		t.Fatal("empty repeat woke the dispatcher")
	default:
	}
	jobs, err := store.claim(t.Context(), opens, count)
	if err != nil || len(jobs) != count {
		t.Fatalf("claimed %d burst reminders, err = %v", len(jobs), err)
	}
	var gotIDs []string
	for _, job := range jobs {
		if job.owner != owner || job.target.Platform != device.Platform || job.target.Token != device.Token {
			t.Fatalf("unexpected burst recipient: %+v", job.target)
		}
		gotIDs = append(gotIDs, job.message.FlightID)
	}
	slices.Sort(wantIDs)
	slices.Sort(gotIDs)
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("queued burst IDs = %v, want %v", gotIDs, wantIDs)
	}
}

func TestCheckInSchedulerFillsPageAcrossDepartureKeys(t *testing.T) {
	opens := time.Date(2027, time.May, 18, 12, 0, 0, 0, time.UTC)
	store, owner, flight, _ := seedFutureCheckIn(t, opens)
	earlier := flight
	earlier.ScheduledDeparture = new(flight.ScheduledDeparture.Add(-30 * time.Minute))
	earlierIDs := addFutureCheckIns(t, store, owner, earlier, "earlier", 20)
	laterIDs := append([]string{flight.ID}, addFutureCheckIns(t, store, owner, flight, "later", 17)...)
	checkInSchedulerCounts(t, store, 0, 0, 0)
	ids := func(candidates []checkInCandidate) []string {
		var result []string
		for _, candidate := range candidates {
			result = append(result, candidate.id)
		}
		return result
	}
	first, err := store.dueCheckInFlights(t.Context(), opens, nil)
	if err != nil || !slices.Equal(ids(first), earlierIDs[:checkInSchedulerBatch]) {
		t.Fatalf("first page = %v, error = %v", ids(first), err)
	}
	last := first[len(first)-1]
	second, err := store.dueCheckInFlights(t.Context(), opens, &last)
	wantSecond := append(slices.Clone(earlierIDs[checkInSchedulerBatch:]), laterIDs[:12]...)
	if err != nil || len(second) != checkInSchedulerBatch || !slices.Equal(ids(second), wantSecond) {
		t.Fatalf("boundary page = %v, want %v, error = %v", ids(second), wantSecond, err)
	}
	last = second[len(second)-1]
	third, err := store.dueCheckInFlights(t.Context(), opens, &last)
	if err != nil || !slices.Equal(ids(third), laterIDs[12:]) {
		t.Fatalf("final page = %v, want %v, error = %v", ids(third), laterIDs[12:], err)
	}
	if err := store.queueDueCheckIns(t.Context(), opens); err != nil {
		t.Fatal(err)
	}
	count := len(earlierIDs) + len(laterIDs)
	checkInSchedulerCounts(t, store, count, count, count)
}

func TestCheckInSchedulerDrainsPastRoundedClosedPrefix(t *testing.T) {
	opens := time.Date(2027, time.May, 18, 12, 0, 0, 0, time.UTC)
	store, owner, flight, _ := seedFutureCheckIn(t, opens)
	closed := flight
	closed.ScheduledDeparture = new(flight.ScheduledDeparture.Add(time.Nanosecond))
	addFutureCheckIns(t, store, owner, closed, "000-closed", checkInSchedulerBatch)
	const dueCount = 2*checkInSchedulerBatch + 1
	addFutureCheckIns(t, store, owner, flight, "later", dueCount-1)
	first, err := store.dueCheckInFlights(t.Context(), opens, nil)
	if err != nil || len(first) != checkInSchedulerBatch {
		t.Fatalf("closed prefix = %v, err = %v", first, err)
	}
	for _, candidate := range first {
		if !strings.HasPrefix(candidate.id, "000-closed-") {
			t.Fatalf("unexpected closed-prefix candidate: %+v", candidate)
		}
	}
	next, err := store.dueCheckInFlights(t.Context(), opens, &first[len(first)-1])
	if err != nil || len(next) == 0 || next[0].departureDay != first[0].departureDay {
		t.Fatalf("rounded SQL key did not tie across pages: first = %v, next = %v, err = %v", first, next, err)
	}
	if err := store.queueDueCheckIns(t.Context(), opens); err != nil {
		t.Fatal(err)
	}
	checkInSchedulerCounts(t, store, dueCount, dueCount, dueCount)
	var closedEvents int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM events WHERE flight_id LIKE '000-closed-%'`).Scan(&closedEvents); err != nil || closedEvents != 0 {
		t.Fatalf("closed-prefix events = %d, err = %v", closedEvents, err)
	}
	if err := store.queueDueCheckIns(t.Context(), opens.Add(time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	const total = dueCount + checkInSchedulerBatch
	checkInSchedulerCounts(t, store, total, total, total)
}

func TestCheckInSchedulerDrainsPastUndecodablePrefix(t *testing.T) {
	opens := time.Date(2027, time.May, 18, 12, 0, 0, 0, time.UTC)
	store, owner, flight, _ := seedFutureCheckIn(t, opens)
	badIDs := addFutureCheckIns(t, store, owner, flight, "000-broken", checkInSchedulerBatch)
	const dueCount = 2*checkInSchedulerBatch + 1
	addFutureCheckIns(t, store, owner, flight, "later", dueCount-1)
	// Keep SQL JSON extraction/indexing valid while making Flight decoding fail.
	if _, err := store.db.ExecContext(t.Context(), `UPDATE flights SET snapshot=json_set(snapshot,'$.number',42) WHERE id LIKE '000-broken-%'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		err := store.queueDueCheckIns(t.Context(), opens)
		if err == nil || !strings.Contains(err.Error(), badIDs[0]) {
			t.Fatalf("first broken flight error = %v, want %s", err, badIDs[0])
		}
		checkInSchedulerCounts(t, store, dueCount, dueCount, dueCount)
	}
}

func TestCheckInSchedulerDrainsPastRolledBackFlightAndRetries(t *testing.T) {
	opens := time.Date(2027, time.May, 18, 12, 0, 0, 0, time.UTC)
	store, owner, flight, _ := seedFutureCheckIn(t, opens)
	const laterCount = 2*checkInSchedulerBatch + 1
	addFutureCheckIns(t, store, owner, flight, "later", laterCount)
	if _, err := store.db.ExecContext(t.Context(), `
CREATE TRIGGER reject_check_in BEFORE INSERT ON outbox WHEN new.flight_id='check-in-scheduler'
BEGIN SELECT RAISE(ABORT, 'test check-in failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := store.queueDueCheckIns(t.Context(), opens); err == nil || !strings.Contains(err.Error(), "test check-in failure") {
		t.Fatalf("check-in insertion failure = %v", err)
	}
	checkInSchedulerCounts(t, store, laterCount, laterCount, laterCount)
	var partialEvents, partialLatches int
	if err := store.db.QueryRowContext(t.Context(), `SELECT
 (SELECT COUNT(*) FROM events WHERE flight_id=?),
 (SELECT COUNT(*) FROM check_in_queued WHERE flight_id=?)`, flight.ID, flight.ID).Scan(&partialEvents, &partialLatches); err != nil || partialEvents != 0 || partialLatches != 0 {
		t.Fatalf("rolled-back flight events/latches = %d/%d, err = %v", partialEvents, partialLatches, err)
	}
	if _, err := store.db.ExecContext(t.Context(), `DROP TRIGGER reject_check_in`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.queueDueCheckIns(t.Context(), opens); err != nil {
			t.Fatal(err)
		}
		checkInSchedulerCounts(t, store, laterCount+1, laterCount+1, laterCount+1)
	}
}

func TestCheckInSchedulerCancellation(t *testing.T) {
	opens := time.Date(2027, time.May, 18, 12, 0, 0, 0, time.UTC)
	t.Run("before query", func(t *testing.T) {
		store, _, _, _ := seedFutureCheckIn(t, opens)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := store.queueDueCheckIns(ctx, opens); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled scan = %v", err)
		}
		checkInSchedulerCounts(t, store, 0, 0, 0)
	})
	t.Run("last candidate overrides prior error", func(t *testing.T) {
		store, owner, flight, _ := seedFutureCheckIn(t, opens)
		addFutureCheckIns(t, store, owner, flight, "000-broken", 1)
		if _, err := store.db.ExecContext(t.Context(), `UPDATE flights SET snapshot=json_set(snapshot,'$.number',42) WHERE id='000-broken-00'`); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		connection, err := store.db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// Cancel inside the final candidate's real write, after the earlier
		// decode error has been saved. This page contains only two flights.
		err = connection.Raw(func(raw any) error {
			return raw.(*sqlite3.SQLiteConn).RegisterFunc("cancel_check_in", func() int {
				cancel()
				return 0
			}, false)
		})
		if closeErr := connection.Close(); err != nil || closeErr != nil {
			t.Fatalf("register cancellation fixture: %v, close: %v", err, closeErr)
		}
		if _, err := store.db.ExecContext(t.Context(), `
CREATE TRIGGER cancel_last_check_in BEFORE INSERT ON outbox WHEN new.flight_id='check-in-scheduler'
BEGIN SELECT cancel_check_in(); END;`); err != nil {
			t.Fatal(err)
		}
		if err := store.queueDueCheckIns(ctx, opens); !errors.Is(err, context.Canceled) {
			t.Fatalf("last-candidate cancellation lost to saved error: %v", err)
		}
		checkInSchedulerCounts(t, store, 0, 0, 0)
	})
	t.Run("scheduler terminates", func(t *testing.T) {
		store, _, _, _ := seedFutureCheckIn(t, opens)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			store.runCheckInScheduler(ctx, slog.New(slog.DiscardHandler))
		}()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled scheduler did not terminate")
		}
	})
}

func TestCheckInSchedulerReoffersOnlyUnsentRescheduledReminder(t *testing.T) {
	for _, tc := range []struct {
		name               string
		claimed, submitted bool
	}{
		{name: "unclaimed"},
		{name: "claimed but unsent", claimed: true},
		{name: "possibly submitted", claimed: true, submitted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			opens := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
			store, _, flight, _ := seedFutureCheckIn(t, opens)
			if err := store.queueDueCheckIns(ctx, opens); err != nil {
				t.Fatal(err)
			}
			if tc.claimed {
				jobs, err := store.claim(ctx, opens, 1)
				if err != nil || len(jobs) != 1 {
					t.Fatalf("claimed %d reminders, err = %v", len(jobs), err)
				}
				if tc.submitted {
					if authorized, err := store.authorizeCheckIn(ctx, jobs[0], opens); err != nil || !authorized {
						t.Fatalf("check-in authorization = %t, err = %v", authorized, err)
					}
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
			if tc.submitted {
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
