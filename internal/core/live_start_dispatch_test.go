package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestDispatcherSkipsLiveStartOutsideDepartureWindow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outside func(*feeds.Flight)
	}{
		{"departure delayed", func(flight *feeds.Flight) {
			delay := flight.ObservedAt.Add(3 * time.Hour)
			flight.EstimatedDeparture = &delay
		}},
		{"departure unknown", func(flight *feeds.Flight) {
			flight.ScheduledDeparture, flight.EstimatedDeparture = nil, nil
		}},
		{"flight cancelled", func(flight *feeds.Flight) {
			flight.Status = "cancelled"
		}},
	} {
		for _, platform := range []string{"ios", "android"} {
			t.Run(platform+"/"+tc.name, func(t *testing.T) {
				fixture := newLiveStartDispatchFixture(t, platform)
				departure := fixture.flight.ScheduledDeparture
				stale := fixture.claimStart(t, fixture.now.Add(time.Second))
				if platform == "android" {
					// Android may queue an update behind a still-leased start.
					fixture.flight.DepartureGate = "A2"
					fixture.observe(t, 2*time.Second)
					var queued bool
					if err := fixture.store.db.QueryRowContext(t.Context(), `SELECT EXISTS(
SELECT 1 FROM outbox WHERE device_id=? AND flight_id=? AND done=0 AND json_extract(message,'$.Kind')='update'
)`, stale.deviceID, fixture.flight.ID).Scan(&queued); err != nil || !queued {
						t.Fatalf("expected an update queued behind the start, got %t, %v", queued, err)
					}
				}
				tc.outside(&fixture.flight)
				fixture.observe(t, 3*time.Second)
				fixture.deliver(t, stale, nil)
				if len(fixture.sent) != 0 {
					t.Fatalf("ineligible live start reached transport: %+v", fixture.sent)
				}
				if jobs := fixture.claimLive(t, fixture.flight.ObservedAt); len(jobs) != 0 {
					t.Fatalf("dropping the start exposed dependent progress: %+v", jobs)
				}

				fixture.flight.ScheduledDeparture, fixture.flight.EstimatedDeparture = departure, nil
				fixture.flight.Status = "scheduled"
				fixture.flight.DepartureGate = "B2"
				fixture.observe(t, 4*time.Second)
				fresh := fixture.claimStart(t, fixture.flight.ObservedAt)
				if fresh.id == stale.id {
					t.Fatal("expected the stale job to be replaced by a new start")
				}
				fixture.deliver(t, fresh, nil)
				if len(fixture.sent) != 1 || fixture.sent[0].Kind != push.LiveStart || fixture.sent[0].State == nil ||
					fixture.sent[0].State.DepartureGate != "B2" || fixture.sent[0].State.DepartureEpoch != departure.Unix() {
					t.Fatalf("replacement did not send the latest live state: %+v", fixture.sent)
				}
			})
		}
	}
}

func TestDispatcherRefreshesUnsubmittedEligibleIOSLiveStart(t *testing.T) {
	for _, platform := range []string{"ios", "android"} {
		t.Run(platform, func(t *testing.T) {
			fixture := newLiveStartDispatchFixture(t, platform)
			job := fixture.claimStart(t, fixture.now.Add(time.Second))
			if err := fixture.store.Track(t.Context(), fixture.owner, fixture.flight.ID, "14A"); err != nil {
				t.Fatal(err)
			}
			estimate := fixture.now.Add(40 * time.Minute)
			fixture.flight.EstimatedDeparture, fixture.flight.DepartureGate = &estimate, "B2"
			fixture.observe(t, 2500*time.Millisecond)
			fixture.deliver(t, job, nil)
			if len(fixture.sent) != 1 {
				t.Fatalf("eligible start sent %d messages, want 1", len(fixture.sent))
			}
			got := fixture.sent[0]
			if platform == "android" {
				if got.State == nil || job.message.State == nil || *got.State != *job.message.State ||
					got.LiveObservedNs != job.message.LiveObservedNs {
					t.Fatalf("Android start lost its original observation: %+v", got)
				}
				followers := fixture.claimLive(t, fixture.flight.ObservedAt)
				if len(followers) != 1 || followers[0].message.Kind != push.LiveUpdate ||
					followers[0].message.State.DepartureGate != "B2" || followers[0].message.State.Seat != "14A" ||
					followers[0].message.LiveObservedNs != fixture.flight.ObservedAt.UnixNano() {
					t.Fatalf("Android start did not preserve its newer update: %+v", followers)
				}
				return
			}
			if got.Kind != push.LiveStart || got.State == nil || got.State.DepartureGate != "B2" || got.State.Seat != "14A" ||
				got.State.DepartureEpoch != estimate.Unix() || got.State.ObservedEpoch != fixture.flight.ObservedAt.Unix() ||
				got.LiveObservedNs != fixture.flight.ObservedAt.UnixNano() {
				t.Fatalf("live start did not use current state: %+v, state %+v", got, got.State)
			}
			if !got.Timestamp.Equal(job.message.Timestamp) || !got.ExpiresAt.Equal(job.message.ExpiresAt) {
				t.Fatalf("refresh changed wire timestamp or expiry: original %+v, sent %+v", job.message, got)
			}
		})
	}
}

func TestDispatcherSkipsClaimedRemoteStartAfterLocalActivityRegisters(t *testing.T) {
	fixture := newLiveStartDispatchFixture(t, "ios")
	job := fixture.claimStart(t, fixture.now.Add(time.Second))
	device := Device{Platform: "ios", Kind: "live_update", Token: "fixture-local-update",
		FlightID: fixture.flight.ID, Binding: fixture.device.Binding, LiveSlot: "activity:" + strings.Repeat("a", 64), LiveRevision: 2}
	if err := fixture.store.Register(t.Context(), fixture.owner, device, fixture.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.deliver(t, job, nil)
	if len(fixture.sent) != 0 {
		t.Fatalf("duplicate remote start reached transport: %+v", fixture.sent)
	}
	jobs := fixture.claimLive(t, fixture.now.Add(3*time.Second))
	if len(jobs) != 1 || jobs[0].target.Token != device.Token || jobs[0].message.Kind != push.LiveUpdate {
		t.Fatalf("local activity should retain its own update: %+v", jobs)
	}
}

func TestDispatcherKeepsPossiblySubmittedLiveStartLatchWhenRetryBecomesIneligible(t *testing.T) {
	for _, platform := range []string{"ios", "android"} {
		t.Run(platform, func(t *testing.T) {
			fixture := newLiveStartDispatchFixture(t, platform)
			first := fixture.claimStart(t, fixture.now.Add(time.Second))
			fixture.deliver(t, first, &push.DeliveryError{Code: "temporary fixture", Retryable: true})
			if len(fixture.sent) != 1 {
				t.Fatalf("first start sent %d messages, want 1", len(fixture.sent))
			}

			delay := fixture.now.Add(3 * time.Hour)
			fixture.flight.EstimatedDeparture = &delay
			fixture.observe(t, 2*time.Second)
			retry := fixture.claimStart(t, time.Now().Add(2*time.Second))
			if retry.id != first.id || retry.attempts != 1 {
				t.Fatalf("expected a retry of the submitted start, got %+v", retry)
			}
			fixture.deliver(t, retry, nil)
			if len(fixture.sent) != 1 {
				t.Fatalf("ineligible retry reached transport: %+v", fixture.sent)
			}
			var latched bool
			if err := fixture.store.db.QueryRowContext(t.Context(), `SELECT EXISTS(
SELECT 1 FROM live_started WHERE device_id=? AND flight_id=?
)`, first.deviceID, fixture.flight.ID).Scan(&latched); err != nil || !latched {
				t.Fatalf("possibly submitted start lost its latch: got %t, %v", latched, err)
			}
		})
	}
}

type liveStartDispatchFixture struct {
	store  *Store
	owner  string
	device Device
	flight feeds.Flight
	now    time.Time
	sent   []push.Message
}

func newLiveStartDispatchFixture(t *testing.T, platform string) *liveStartDispatchFixture {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "live-start-dispatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Add(-6 * time.Second)
	departure := now.Add(30 * time.Minute)
	fixture := &liveStartDispatchFixture{
		store: store,
		now:   now,
		flight: feeds.Flight{ID: "start-flight", Number: "UA123", Origin: "SFO", Destination: "JFK",
			Status: "scheduled", ScheduledDeparture: &departure, DepartureGate: "A1"},
		device: Device{Platform: platform, Kind: "alert", Token: "fixture-device", Binding: "0af06679-3c08-4f26-9a5b-15f31d4904ab"},
	}
	if platform == "ios" {
		fixture.device.Kind, fixture.device.LiveSlot, fixture.device.LiveRevision = "live_start", "start", 1
	}
	fixture.observe(t, 0)
	fixture.owner = slotInstallation(t, store)
	if err := store.Track(t.Context(), fixture.owner, fixture.flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(t.Context(), fixture.owner, fixture.device, now); err != nil {
		t.Fatal(err)
	}
	fixture.observe(t, time.Second)
	return fixture
}

func (f *liveStartDispatchFixture) observe(t *testing.T, offset time.Duration) {
	t.Helper()
	f.flight.ObservedAt = f.now.Add(offset)
	if err := f.store.Observe(t.Context(), f.flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func (f *liveStartDispatchFixture) claimLive(t *testing.T, at time.Time) []deliveryJob {
	t.Helper()
	ctx := t.Context()
	jobs, err := f.store.claimForPlatform(ctx, at, 16, f.device.Platform)
	if err != nil {
		t.Fatal(err)
	}
	var live []deliveryJob
	for _, job := range jobs {
		// Android also registers for independent passenger alerts.
		if job.message.Kind == push.Alert {
			if err := f.store.resolve(ctx, job, nil, at); err != nil {
				t.Fatal(err)
			}
		} else {
			live = append(live, job)
		}
	}
	return live
}

func (f *liveStartDispatchFixture) claimStart(t *testing.T, at time.Time) deliveryJob {
	t.Helper()
	jobs := f.claimLive(t, at)
	if len(jobs) != 1 || jobs[0].message.Kind != push.LiveStart || jobs[0].target.Token != f.device.Token {
		t.Fatalf("expected one live start, got %+v", jobs)
	}
	return jobs[0]
}

func (f *liveStartDispatchFixture) deliver(t *testing.T, job deliveryJob, sendErr error) {
	t.Helper()
	sender := pushFunc(func(_ context.Context, _ push.Target, message push.Message) error {
		f.sent = append(f.sent, message)
		return sendErr
	})
	f.store.deliver(t.Context(), sender, slog.New(slog.DiscardHandler), job)
}
