package core

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/push"
)

func TestDispatcherReservesAlertCapacityDuringLiveUpdates(t *testing.T) {
	store, owner, id := dispatcherFixture(t)
	flight, err := store.Flight(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	departure := now.Add(30 * time.Hour)
	flight.ScheduledDeparture, flight.DepartureGate, flight.ObservedAt = &departure, "A1", now
	if err := store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(t.Context(), owner, id, ""); err != nil {
		t.Fatal(err)
	}

	const providerSlots = 8
	const generalSlots = 6
	for i := range providerSlots {
		device := Device{Platform: "ios", Kind: "live_update", FlightID: id, Token: fmt.Sprintf("live-update-%d", i)}
		if err := store.Register(t.Context(), owner, device, now); err != nil {
			t.Fatal(err)
		}
	}
	const alertToken = "gate-alert"
	if err := store.Register(t.Context(), owner, Device{Platform: "ios", Kind: "alert", Token: alertToken}, now); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	started, done := make(chan struct{}, providerSlots), make(chan struct{})
	type delivery struct {
		target  push.Target
		message push.Message
	}
	alerts := make(chan delivery, 1)
	sender := pushFunc(func(ctx context.Context, target push.Target, message push.Message) error {
		if message.Kind == push.LiveUpdate {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		select {
		case alerts <- delivery{target: target, message: message}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	go func() {
		defer close(done)
		store.RunDispatcher(ctx, sender, slog.New(slog.DiscardHandler))
	}()
	defer func() { cancel(); <-done }()
	for range generalSlots {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("live updates did not fill the provider's general capacity")
		}
	}
	var leased int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE lease_until>0`).Scan(&leased); err != nil || leased != generalSlots {
		t.Fatalf("leased live updates = %d, error = %v, want %d", leased, err, generalSlots)
	}

	flight.DepartureGate, flight.ObservedAt = "B2", time.Now()
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-alerts:
		if d.target.Platform != "ios" || d.target.Token != alertToken || d.message.Kind != push.Alert ||
			d.message.FlightID != id || d.message.Body != "AA2 · B2" {
			t.Fatalf("unexpected delivery %+v", d)
		}
	case <-ctx.Done():
		t.Fatal("gate alert waited behind live updates for the same provider")
	}
}
