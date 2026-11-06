package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestObservedChangesAndOutboxSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	now := time.Now().UTC()
	departure, arrival := now.Add(30*time.Hour), now.Add(33*time.Hour)
	f := feeds.Flight{ID: "test-1", Number: "XY1", Origin: "SFO", Destination: "JFK", Status: "scheduled", ScheduledDeparture: &departure, ScheduledArrival: &arrival, ObservedAt: now}
	if err := s.Observe(ctx, f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, f.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ctx, owner, Device{Platform: "ios", Token: "test-push-token", Kind: "alert"}, now); err != nil {
		t.Fatal(err)
	}
	f.DepartureGate = "C1"
	f.ObservedAt = now.Add(time.Second)
	if err := s.Observe(ctx, f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Observe(ctx, f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.claim(ctx, now.Add(2*time.Second), 16)
	if err != nil || len(jobs) != 1 || jobs[0].message.Kind != push.Alert || jobs[0].message.Title != "Departure gate" {
		t.Fatalf("unexpected outbox: %v (%v)", jobs, err)
	}
	if err := s.resolve(ctx, jobs[0], &push.DeliveryError{Retryable: true}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	jobs, err = s.claim(ctx, now.Add(4*time.Second), 16)
	if err != nil || len(jobs) != 1 || jobs[0].attempts != 1 {
		t.Fatalf("retry: %v (%v)", jobs, err)
	}
	if err := s.resolve(ctx, jobs[0], nil, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	jobs, err = s.claim(ctx, now.Add(6*time.Second), 16)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("resolved job claimed again: %v (%v)", jobs, err)
	}
}

func TestCheckInQueuesForLateRegistrationOnlyOnce(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	departure := now.Add(time.Hour)
	flight := feeds.Flight{ID: "demo-check-in", Number: "XY1", Origin: "JFK", Destination: "LHR", Status: "scheduled", ScheduledDeparture: &departure, ObservedAt: now}
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	device := Device{Platform: "ios", Token: "check-in-test-token", Kind: "alert"}
	for _, at := range []time.Time{now.Add(time.Second), now.Add(2 * time.Second)} {
		if err := s.Register(ctx, owner, device, at); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := s.claim(ctx, now.Add(3*time.Second), 16)
	if err != nil || len(jobs) != 1 || jobs[0].message.Title != "Check-in reminder" {
		t.Fatalf("unexpected reminder jobs: %v (%v)", jobs, err)
	}
}

func TestLocalLiveRegistrationSupersedesPendingRemoteStart(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	departure := now.Add(40 * time.Minute)
	flight := feeds.Flight{ID: "demo-live", Number: "XY1", Origin: "JFK", Destination: "LHR", Status: "scheduled", ScheduledDeparture: &departure, ObservedAt: now}
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, flight.ID, "12A"); err != nil {
		t.Fatal(err)
	}
	start := Device{Platform: "ios", Token: "live-start-test-token", Kind: "live_start", RequestUpdateToken: true}
	if err := s.Register(ctx, owner, start, now); err != nil {
		t.Fatal(err)
	}
	flight.ObservedAt = now.Add(time.Second)
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var marker int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE json_extract(message,'$.RequestUpdateToken')=1`).Scan(&marker); err != nil || marker != 1 {
		t.Fatalf("missing token request flag: %d (%v)", marker, err)
	}
	update := Device{Platform: "ios", Token: "live-update-test-token", Kind: "live_update", FlightID: flight.ID}
	if err := s.Register(ctx, owner, update, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.claim(ctx, now.Add(3*time.Second), 16)
	if err != nil || len(jobs) != 1 || jobs[0].message.Kind != push.LiveUpdate || jobs[0].message.State.Seat != "12A" {
		t.Fatalf("unexpected live jobs: %v (%v)", jobs, err)
	}
}

func TestAPNSLiveWritesUseDistinctOrderedWireTimestamps(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "ordered-live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	dep := now.Add(20 * time.Minute)
	f := feeds.Flight{ID: "live-ordered", Origin: "JFK", Destination: "EWR", Number: "UA2", Status: "scheduled", ScheduledDeparture: &dep, ObservedAt: now}
	if err := s.Observe(ctx, f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, f.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ctx, owner, Device{Platform: "ios", Token: "same-live-token", Kind: "live_update", FlightID: f.ID}, now); err != nil {
		t.Fatal(err)
	}
	first, err := s.claim(ctx, now, 8)
	if err != nil || len(first) != 1 {
		t.Fatalf("first %v %v", first, err)
	}
	f.DepartureGate, f.ObservedAt = "A1", now.Add(100*time.Millisecond)
	if err := s.Observe(ctx, f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	next := now.Add(time.Second)
	if jobs, err := s.claim(ctx, next, 8); err != nil || len(jobs) != 0 {
		t.Fatalf("parallel live job %v %v", jobs, err)
	}
	if err := s.resolve(ctx, first[0], nil, next); err != nil {
		t.Fatal(err)
	}
	second, err := s.claim(ctx, next, 8)
	if err != nil || len(second) != 1 || second[0].message.Timestamp.Unix() <= first[0].message.Timestamp.Unix() || second[0].message.State.DepartureGate != "A1" {
		t.Fatalf("second %v %v", second, err)
	}
}

func TestAndroidBindingRotationKeepsOldDeliveriesAndRevocationScoped(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "binding.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	dep := now.Add(30 * time.Hour)
	flight := feeds.Flight{ID: "binding-test", Number: "UA55", Origin: "LAX", Destination: "SFO", Status: "scheduled", DepartureGate: "A1", ObservedAt: now, ScheduledDeparture: &dep}
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	oldDevice := Device{Platform: "android", Kind: "alert", Token: "binding-fixture", Binding: "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"}
	if err := s.Register(ctx, owner, oldDevice, now); err != nil {
		t.Fatal(err)
	}
	flight.DepartureGate, flight.ObservedAt = "B2", now.Add(time.Second)
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	old, err := s.claim(ctx, flight.ObservedAt, 8)
	if err != nil || len(old) != 1 || old[0].target.Binding != oldDevice.Binding {
		t.Fatalf("old %v %v", old, err)
	}
	newDevice := oldDevice
	newDevice.Binding = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	if err := s.Register(ctx, owner, newDevice, flight.ObservedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveDevice(ctx, owner, oldDevice); err != nil {
		t.Fatal(err)
	}
	oldDevice.Token = ""
	if err := s.RemoveDevice(ctx, owner, oldDevice); err != nil {
		t.Fatal(err)
	}
	if err := s.resolve(ctx, old[0], &push.DeliveryError{InvalidToken: true}, flight.ObservedAt); err != nil {
		t.Fatal(err)
	}
	flight.DepartureGate, flight.ObservedAt = "C3", now.Add(2*time.Second)
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	newJobs, err := s.claim(ctx, flight.ObservedAt, 8)
	if err != nil || len(newJobs) != 1 || newJobs[0].target.Binding != newDevice.Binding || old[0].deviceID == newJobs[0].deviceID {
		t.Fatalf("new %v %v", newJobs, err)
	}
}
