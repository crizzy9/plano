package core

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestLiveProgressStartsFromObservedDeparture(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	planned, gateOut, takeoff, arrival := now.Add(-2*time.Hour), now.Add(-25*time.Minute), now.Add(-10*time.Minute), now.Add(2*time.Hour)
	flight := feeds.Flight{ID: "live-flight", Number: "UA123", Status: "airborne", ObservedAt: now,
		ScheduledDeparture: &planned, ActualDeparture: &gateOut, ActualTakeoff: &takeoff, EstimatedArrival: &arrival}
	for _, want := range []time.Time{gateOut, takeoff, planned} {
		state := liveMessage(flight, push.LiveUpdate, "", now).State
		if state.DepartureEpoch != want.Unix() || state.ArrivalEpoch != arrival.Unix() {
			t.Fatalf("progress time window %d to %d, want %d to %d", state.DepartureEpoch, state.ArrivalEpoch, want.Unix(), arrival.Unix())
		}
		if flight.ActualDeparture != nil {
			flight.ActualDeparture = nil
		} else {
			flight.ActualTakeoff = nil
		}
	}
}

func TestLiveHeartbeatWithFiveSecondObservations(t *testing.T) {
	for _, platform := range []string{"ios", "android"} {
		t.Run(platform, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "heartbeat.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			now := time.Now().UTC().Truncate(time.Second).Add(123456789 * time.Nanosecond)
			departure, arrival := now.Add(-6*time.Hour), now.Add(2*time.Hour)
			flight := feeds.Flight{ID: "long-haul", Number: "UA123", Origin: "SFO", Destination: "LHR",
				Status: "airborne", ScheduledDeparture: &departure, EstimatedArrival: &arrival, ObservedAt: now.Add(-time.Second)}
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			owner := slotInstallation(t, store)
			if err := store.Track(ctx, owner, flight.ID, "8A"); err != nil {
				t.Fatal(err)
			}
			device := Device{Platform: platform, Kind: "alert", Token: "heartbeat-" + platform}
			initial := push.LiveStart
			if platform == "ios" {
				device.Kind, device.FlightID = "live_update", flight.ID
				initial = push.LiveUpdate
			}
			if err := store.Register(ctx, owner, device, now); err != nil {
				t.Fatal(err)
			}
			observe := func(offset time.Duration, want ...push.Kind) {
				t.Helper()
				flight.ObservedAt = now.Add(offset)
				if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				jobs := liveJobsAt(t, store, flight.ObservedAt)
				if len(jobs) != len(want) {
					t.Fatalf("at %v: got %d live jobs, want %v", offset, len(jobs), want)
				}
				for i, job := range jobs {
					if job.message.Kind != want[i] || job.target.Platform != platform {
						t.Fatalf("at %v: got %s on %s, want %s on %s", offset, job.message.Kind, job.target.Platform, want[i], platform)
					}
					if offset > 0 && (job.message.State.ObservedEpoch != flight.ObservedAt.Unix() ||
						job.message.LiveObservedNs != flight.ObservedAt.UnixNano()) {
						t.Fatalf("at %v: job used an older observation", offset)
					}
					if heartbeat := platform == "android" && offset == time.Minute; job.message.AndroidLiveHeartbeat != heartbeat {
						t.Fatalf("at %v: Android heartbeat = %t, want %t", offset, job.message.AndroidLiveHeartbeat, heartbeat)
					}
				}
			}
			observe(0, initial)
			for offset := 5 * time.Second; offset < time.Minute; offset += 5 * time.Second {
				observe(offset)
				if offset == 30*time.Second {
					if err := store.Close(); err != nil {
						t.Fatal(err)
					}
					store, err = Open(path)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			observe(time.Minute, push.LiveUpdate)
			flight.ArrivalGate = "B12"
			observe(65*time.Second, push.LiveUpdate)
			observe(70 * time.Second)
			flight.Status, flight.ScheduledDeparture = "arrived", nil
			observe(75*time.Second, push.LiveEnd)
			observe(80 * time.Second)
			if platform == "android" {
				flight.Status, flight.ScheduledDeparture = "airborne", &departure
				observe(85*time.Second, push.LiveUpdate)
			}
		})
	}
}

func TestAndroidLiveHeartbeatPreservesPendingChange(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "heartbeat-retry.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	flight := feeds.Flight{ID: "heartbeat-retry", Number: "UA123", Origin: "SFO", Destination: "LHR",
		Status: "airborne", ObservedAt: now}
	observe := func(at time.Time) {
		t.Helper()
		flight.ObservedAt = at
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	observe(now)
	owner := slotInstallation(t, store)
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, owner, Device{Platform: "android", Kind: "alert", Token: "heartbeat-retry-token"}, now); err != nil {
		t.Fatal(err)
	}
	observe(now.Add(time.Second))
	if jobs := liveJobsAt(t, store, flight.ObservedAt); len(jobs) != 1 || jobs[0].message.Kind != push.LiveStart ||
		jobs[0].message.LiveObservedNs != flight.ObservedAt.UnixNano() {
		t.Fatalf("expected a delivered start, got %v", jobs)
	}
	flight.ArrivalGate = "B12"
	changedAt := flight.ObservedAt.Add(200 * time.Millisecond)
	observe(changedAt)
	jobs, err := store.claim(ctx, changedAt, 16)
	if err != nil {
		t.Fatal(err)
	}
	var changeID int64
	for _, job := range jobs {
		var sendErr error
		if job.message.Kind == push.LiveUpdate && !job.message.AndroidLiveHeartbeat {
			changeID = job.id
			sendErr = &push.DeliveryError{Retryable: true, RetryAfter: 2 * time.Minute}
		}
		if err := store.resolve(ctx, job, sendErr, changedAt); err != nil {
			t.Fatal(err)
		}
	}
	if changeID == 0 {
		t.Fatal("expected a meaningful Android change to retry")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store = reopened
	observe(changedAt.Add(time.Minute))
	if jobs := liveJobsAt(t, store, flight.ObservedAt); len(jobs) != 0 {
		t.Fatalf("a heartbeat displaced the pending change: %v", jobs)
	}
	if jobs := liveJobsAt(t, store, changedAt.Add(2*time.Minute)); len(jobs) != 1 || jobs[0].id != changeID ||
		jobs[0].message.AndroidLiveHeartbeat || jobs[0].message.State.ArrivalGate != "B12" ||
		jobs[0].message.State.ObservedEpoch != changedAt.Unix() || jobs[0].message.LiveObservedNs != changedAt.UnixNano() {
		t.Fatalf("expected to retry the original change, got %v", jobs)
	}
}

func TestLiveEligibilityWithoutGateDeparture(t *testing.T) {
	for _, test := range []struct {
		status      string
		schedule    time.Duration
		hasSchedule bool
		shouldStart bool
	}{
		{status: "taxi_out", schedule: -3 * time.Hour, hasSchedule: true, shouldStart: true},
		{status: "airborne", shouldStart: true},
		{status: "landed", schedule: -8 * time.Hour, hasSchedule: true, shouldStart: true},
		{status: "diverted", schedule: -6 * time.Hour, hasSchedule: true, shouldStart: true},
		{status: "boarding", schedule: -3 * time.Hour, hasSchedule: true},
		{status: "scheduled", schedule: 2 * time.Hour, hasSchedule: true},
	} {
		t.Run(test.status, func(t *testing.T) {
			ctx := t.Context()
			store, err := Open(filepath.Join(t.TempDir(), "eligibility.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			now := time.Now().UTC().Truncate(time.Second)
			flight := feeds.Flight{ID: "flight", Number: "UA123", Origin: "SFO", Destination: "LHR",
				Status: test.status, ObservedAt: now}
			if test.hasSchedule {
				departure := now.Add(test.schedule)
				flight.ScheduledDeparture = &departure
			}
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			owner := slotInstallation(t, store)
			if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
				t.Fatal(err)
			}
			for _, d := range []Device{
				{Platform: "ios", Kind: "live_start", Token: "ios-start"},
				{Platform: "android", Kind: "alert", Token: "android-start"},
			} {
				if err := store.Register(ctx, owner, d, now); err != nil {
					t.Fatal(err)
				}
			}
			flight.ObservedAt = now.Add(5 * time.Second)
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			jobs := liveJobsAt(t, store, flight.ObservedAt)
			if !test.shouldStart {
				if len(jobs) != 0 {
					t.Fatalf("unexpected Live Activity starts: %d", len(jobs))
				}
				return
			}
			if len(jobs) != 2 || jobs[0].message.Kind != push.LiveStart || jobs[1].message.Kind != push.LiveStart {
				t.Fatalf("both devices should start a live surface, got %d jobs", len(jobs))
			}
		})
	}
}

func TestLiveTokenRegistrationKeepsCachedObservationStale(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "stale-activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	flight := feeds.Flight{ID: "stale-flight", Number: "UA123", Origin: "SFO", Destination: "LHR",
		Status: "airborne", ObservedAt: now.Add(-30 * time.Minute)}
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	owner := slotInstallation(t, store)
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	device := Device{Platform: "ios", Kind: "live_update", Token: "stale-activity-token", FlightID: flight.ID}
	if err := store.Register(ctx, owner, device, now); err != nil {
		t.Fatal(err)
	}
	jobs := liveJobsAt(t, store, now)
	if len(jobs) != 1 || jobs[0].message.Kind != push.LiveUpdate {
		t.Fatalf("wanted one live update, got %d jobs", len(jobs))
	}
	msg := jobs[0].message
	if !msg.StaleAt.Equal(flight.ObservedAt.Add(liveFreshness)) || !msg.Timestamp.Equal(now) || !msg.ExpiresAt.After(now) {
		t.Fatalf("cached observation should stay stale on registration, got stale=%s timestamp=%s expiry=%s", msg.StaleAt, msg.Timestamp, msg.ExpiresAt)
	}
	flight.ObservedAt = now.Add(5 * time.Second)
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	jobs = liveJobsAt(t, store, flight.ObservedAt)
	if len(jobs) != 1 || jobs[0].message.Kind != push.LiveUpdate ||
		!jobs[0].message.StaleAt.Equal(flight.ObservedAt.Add(liveFreshness)) {
		t.Fatalf("first fresh observation should recover immediately, got %d jobs", len(jobs))
	}
}

func liveJobsAt(t *testing.T, store *Store, now time.Time) []deliveryJob {
	t.Helper()
	ctx := t.Context()
	jobs, err := store.claim(ctx, now, 16)
	if err != nil {
		t.Fatal(err)
	}
	var live []deliveryJob
	for _, job := range jobs {
		if err := store.resolve(ctx, job, nil, now); err != nil {
			t.Fatal(err)
		}
		if job.message.Kind != push.Alert {
			live = append(live, job)
		}
	}
	return live
}
