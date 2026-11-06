package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

type periodicRefreshSource struct {
	feeds.FlightSource
	get   func(context.Context, string) (feeds.Flight, error)
	track func(context.Context, string) ([]feeds.Position, error)
}

func (s periodicRefreshSource) Get(ctx context.Context, id string) (feeds.Flight, error) {
	return s.get(ctx, id)
}

func (s periodicRefreshSource) Track(ctx context.Context, id string) ([]feeds.Position, error) {
	return s.track(ctx, id)
}

func TestPeriodicRefreshIsolatesSlowAndFailedFlights(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "plano.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	owner := friendInstallation(t, store)
	now := time.Now().UTC()
	departure, arrival := now.Add(time.Hour), now.Add(3*time.Hour)
	updates := make(map[string]feeds.Flight)
	for i, id := range []string{"slow", "broken", "healthy", "later"} {
		flight := feeds.Flight{
			ID: id, Number: "UA123", Origin: "SFO", Destination: "EWR", Status: "scheduled",
			ScheduledDeparture: &departure, ScheduledArrival: &arrival, DepartureGate: "A1",
			ObservedAt: now.Add(time.Duration(i-4) * time.Minute),
		}
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.Track(ctx, owner, id, ""); err != nil {
			t.Fatal(err)
		}
		flight.ObservedAt, flight.DepartureGate = now, "A2"
		updates[id] = flight
	}
	slow := updates["slow"]
	slow.Status, slow.ActualTakeoff = "airborne", new(now.Add(-time.Minute))
	updates[slow.ID] = slow
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER reject_broken_refresh BEFORE UPDATE ON flights WHEN old.id = 'broken'
BEGIN SELECT RAISE(ABORT, 'test observation failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO shares(token_hash, owner, flight_id, expires) VALUES('expired', ?, 'healthy', ?)`,
		owner, now.Add(-time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}

	type providerCall struct {
		id       string
		inFlight int32
	}
	calls := make(chan providerCall, len(updates))
	trackStarted := make(chan int32, 1)
	releaseBroken, releaseTrack := make(chan struct{}), make(chan struct{})
	var inFlight atomic.Int32
	source := periodicRefreshSource{
		get: func(ctx context.Context, id string) (feeds.Flight, error) {
			count := inFlight.Add(1)
			defer inFlight.Add(-1)
			select {
			case calls <- providerCall{id, count}:
			case <-ctx.Done():
				return feeds.Flight{}, ctx.Err()
			}
			if id == "broken" {
				select {
				case <-releaseBroken:
				case <-ctx.Done():
					return feeds.Flight{}, ctx.Err()
				}
			}
			return updates[id], nil
		},
		track: func(ctx context.Context, _ string) ([]feeds.Position, error) {
			count := inFlight.Add(1)
			defer inFlight.Add(-1)
			select {
			case trackStarted <- count:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case <-releaseTrack:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	refresher := Refresher{Store: store, Flights: source, Logger: slog.New(slog.DiscardHandler)}
	done := make(chan struct{})
	var refreshErr error
	go func() {
		refreshErr = refresher.refresh(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	nextCall := func() string {
		t.Helper()
		select {
		case call := <-calls:
			if call.inFlight > 2 {
				t.Fatalf("provider request for %s overlapped with %d requests", call.id, call.inFlight)
			}
			return call.id
		case <-ctx.Done():
			t.Fatalf("waiting for provider request: %v", ctx.Err())
			return ""
		}
	}
	started := []string{nextCall(), nextCall()}
	slices.Sort(started)
	if !slices.Equal(started, []string{"broken", "slow"}) {
		t.Fatalf("first provider requests = %v", started)
	}
	select {
	case count := <-trackStarted:
		if count > 2 {
			t.Fatalf("slow radar request overlapped with %d requests", count)
		}
	case <-ctx.Done():
		t.Fatalf("waiting for slow radar request: %v", ctx.Err())
	}
	close(releaseBroken)
	if got := nextCall(); got != "healthy" {
		t.Fatalf("request after the rejected observation = %s", got)
	}
	// The other worker reaches the fourth flight only after it commits the
	// third. The first flight's radar fetch is still blocked.
	if got := nextCall(); got != "later" {
		t.Fatalf("request after the healthy flight = %s", got)
	}
	details, err := store.Details(ctx, "healthy")
	if err != nil || details.Flight.DepartureGate != "A2" ||
		!slices.ContainsFunc(details.Events, func(event Event) bool { return event.Kind == "departure_gate" && event.At.Equal(now) }) {
		t.Fatalf("healthy flight did not commit its gate event while radar was blocked: %+v, %v", details, err)
	}
	close(releaseTrack)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("waiting for refresh to finish: %v", ctx.Err())
	}
	if refreshErr == nil || !strings.Contains(refreshErr.Error(), "broken") || !strings.Contains(refreshErr.Error(), "test observation failure") {
		t.Fatalf("refresh error = %v", refreshErr)
	}
	broken, err := store.Flight(ctx, "broken")
	if err != nil || broken.DepartureGate != "A1" {
		t.Fatalf("rejected observation changed flight: %+v, %v", broken, err)
	}
	var expiredShares int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shares WHERE token_hash='expired'`).Scan(&expiredShares); err != nil || expiredShares != 0 {
		t.Fatalf("prune left %d expired shares: %v", expiredShares, err)
	}
}
