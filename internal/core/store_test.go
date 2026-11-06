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
