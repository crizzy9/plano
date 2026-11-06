package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestDispatcherRecoversDueCheckInFromSavedFlight(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "check-in.db")
	seed, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	now := time.Now().UTC()
	departure := now.Add(23 * time.Hour)
	observed := now.Add(-2 * time.Hour)
	flight := feeds.Flight{
		ID: "cached-check-in", Number: "AA2", Origin: "JFK", Destination: "SFO",
		Status: "scheduled", ScheduledDeparture: &departure, ObservedAt: observed,
	}
	if err := seed.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	owner := friendInstallation(t, seed)
	for _, platform := range []string{"ios", "android"} {
		device := Device{Platform: platform, Kind: "alert", Token: platform + "-cached-check-in"}
		if err := seed.Register(ctx, owner, device, observed); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := seed.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Track at the saved observation time, before the reminder window.
	if err := trackFlight(ctx, tx, owner, flight.ID, "", observed); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if details, err := seed.Details(ctx, flight.ID); err != nil || len(details.Events) != 0 {
		t.Fatalf("reminder appeared before the window: %+v, error = %v", details.Events, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	type send struct {
		target  push.Target
		message push.Message
	}
	sent := make(chan send, 2)
	sender := pushFunc(func(ctx context.Context, target push.Target, message push.Message) error {
		select {
		case sent <- send{target, message}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.RunDispatcher(ctx, sender, slog.New(slog.DiscardHandler))
	}()
	defer func() { cancel(); <-done }()

	seen := make(map[string]bool, 2)
	for range 2 {
		select {
		case delivery := <-sent:
			platform := delivery.target.Platform
			message := delivery.message
			if seen[platform] || delivery.target.Token != platform+"-cached-check-in" ||
				message.FlightID != flight.ID || message.Kind != push.Alert || message.Title != "Check-in reminder" ||
				message.Timestamp.Before(departure.Add(-24*time.Hour)) ||
				message.ExpiresAt.After(message.Timestamp.Add(2*time.Hour)) {
				t.Fatalf("unexpected check-in delivery: %+v", delivery)
			}
			seen[platform] = true
		case <-ctx.Done():
			t.Fatalf("check-in dispatch did not recover from the saved flight: %v", ctx.Err())
		}
	}
	if !seen["ios"] || !seen["android"] {
		t.Fatalf("delivery platforms = %v", seen)
	}
}
