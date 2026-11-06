package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestAeroHintPersistsAndRequeuesNewGeneration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "aero.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	now := time.Now().UTC()
	f := feeds.Flight{ID: "AAL33-1", Source: "flightaware_aeroapi", ObservedAt: now, Status: "scheduled"}
	if err := s.Observe(ctx, f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAeroRefresh(ctx, f.ID, now); err != nil {
		t.Fatal(err)
	}
	if job, err := s.claimAeroRefresh(ctx, now); err != nil || job != nil {
		t.Fatalf("untracked job %v %v", job, err)
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
	if err := s.QueueAeroRefresh(ctx, f.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.claimAeroRefresh(ctx, now)
	if err != nil || old == nil {
		t.Fatalf("lost persisted job %v %v", old, err)
	}
	later := now.Add(time.Second)
	if err := s.QueueAeroRefresh(ctx, f.ID, later); err != nil {
		t.Fatal(err)
	}
	if err := s.completeAeroRefresh(ctx, old, later, false); err != nil {
		t.Fatal(err)
	}
	newJob, err := s.claimAeroRefresh(ctx, later)
	if err != nil || newJob == nil || newJob.generation != old.generation+1 {
		t.Fatalf("lost concurrent hint %v %v", newJob, err)
	}
	if err := s.completeAeroRefresh(ctx, newJob, later, true); err != nil {
		t.Fatal(err)
	}
	if job, err := s.claimAeroRefresh(ctx, later); err != nil || job != nil {
		t.Fatalf("backoff job %v %v", job, err)
	}
}

type gatedAeroSource struct {
	feeds.FlightSource
	started, release chan struct{}
	flight           feeds.Flight
}

func (source gatedAeroSource) Get(ctx context.Context, _ string) (feeds.Flight, error) {
	close(source.started)
	select {
	case <-source.release:
		return source.flight, nil
	case <-ctx.Done():
		return feeds.Flight{}, ctx.Err()
	}
}

func TestInFlightCallbackDoesNotRevertRemappedAlias(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := Open(filepath.Join(t.TempDir(), "alias.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	local := feeds.Flight{ID: "local-instance", Number: "AA2", Origin: "LAX", Destination: "JFK", Status: "scheduled", ObservedAt: now.Add(-time.Minute), DepartureGate: "B2"}
	if err := s.Observe(ctx, local, nil, nil, nil); err != nil {
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
	if err := s.Track(ctx, owner, local.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO aero_instance_keys VALUES(?,?)`, "AAL2-1", local.ID); err != nil {
		t.Fatal(err)
	}
	remote := local
	remote.ID, remote.Source, remote.ObservedAt, remote.DepartureGate = "AAL2-1", "flightaware_aeroapi", now, "C1"
	source := gatedAeroSource{started: make(chan struct{}), release: make(chan struct{}), flight: remote}
	done := make(chan error, 1)
	go func() { done <- s.refreshAeroFlight(ctx, source, remote.ID) }()
	select {
	case <-source.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE aero_instance_keys SET provider_id=? WHERE flight_id=?`, "AAL2-2", local.ID); err != nil {
		t.Fatal(err)
	}
	close(source.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var key string
	if err := s.db.QueryRowContext(ctx, `SELECT provider_id FROM aero_instance_keys WHERE flight_id=?`, local.ID).Scan(&key); err != nil || key != "AAL2-2" {
		t.Fatalf("alias reverted: %s %v", key, err)
	}
	flight, err := s.Flight(ctx, local.ID)
	if err != nil || flight.DepartureGate != local.DepartureGate {
		t.Fatalf("snapshot overwritten: %#v %v", flight, err)
	}
}
