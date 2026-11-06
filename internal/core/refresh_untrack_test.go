package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestPeriodicRefreshRechecksTrackingBeforeCommit(t *testing.T) {
	for _, tc := range []struct {
		name           string
		anotherTracker bool
		wantGate       string
	}{
		{name: "last tracker removed", wantGate: "A1"},
		{name: "another installation still tracks", anotherTracker: true, wantGate: "A2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store, err := Open(filepath.Join(t.TempDir(), "refresh.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			now := time.Now().UTC()
			flight := feeds.Flight{
				ID: "poll-untrack", Number: "UA123", Origin: "SFO", Destination: "EWR", Status: "scheduled",
				ObservedAt: now.Add(-time.Minute), ScheduledDeparture: new(now.Add(time.Hour)), DepartureGate: "A1",
			}
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			owner := friendInstallation(t, store)
			if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
				t.Fatal(err)
			}
			if tc.anotherTracker {
				if err := store.Track(ctx, friendInstallation(t, store), flight.ID, ""); err != nil {
					t.Fatal(err)
				}
			}
			flight.ObservedAt, flight.DepartureGate = now, "A2"
			source := advisoryRefreshFlights{flight: flight, onGet: func(ctx context.Context) error {
				return store.Untrack(ctx, owner, flight.ID)
			}}
			refresher := Refresher{Store: store, Flights: source, Logger: slog.New(slog.DiscardHandler)}
			if err := refresher.refresh(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := store.Flight(ctx, flight.ID)
			if err != nil || got.DepartureGate != tc.wantGate {
				t.Fatalf("saved gate = %q, want %q, error = %v", got.DepartureGate, tc.wantGate, err)
			}
		})
	}
}
