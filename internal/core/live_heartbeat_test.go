package core

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

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
			now := time.Now().UTC().Truncate(time.Second)
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
					if offset > 0 && job.message.State.ObservedEpoch != flight.ObservedAt.Unix() {
						t.Fatalf("at %v: job used an older observation", offset)
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
		})
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
