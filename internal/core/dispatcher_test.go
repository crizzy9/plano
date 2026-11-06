package core

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

type pushFunc func(context.Context, push.Target, push.Message) error

func (fn pushFunc) Send(ctx context.Context, target push.Target, message push.Message) error {
	return fn(ctx, target, message)
}

func TestDispatcherContinuesWhileAProviderSendIsSlow(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "dispatcher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	now := time.Now().UTC()
	dep := now.Add(30 * time.Hour)
	f := feeds.Flight{ID: "flight-slow-send", Number: "AA2", Origin: "JFK", Destination: "SFO", DepartureGate: "A1", ObservedAt: now.Add(-time.Minute), ScheduledDeparture: &dep, Status: "scheduled"}
	if err := store.Observe(ctx, f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, f.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, owner, Device{Platform: "ios", Kind: "alert", Token: "one-test-device"}, now); err != nil {
		t.Fatal(err)
	}
	first, second, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	sender := pushFunc(func(ctx context.Context, _ push.Target, msg push.Message) error {
		if strings.HasSuffix(msg.Body, "B2") {
			close(first)
			<-ctx.Done()
			return ctx.Err()
		}
		if strings.HasSuffix(msg.Body, "C3") {
			close(second)
		}
		return nil
	})
	go func() {
		defer close(done)
		store.RunDispatcher(ctx, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() { cancel(); <-done }()
	f.DepartureGate, f.ObservedAt = "B2", now.Add(-time.Second)
	if err := store.Observe(ctx, f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("first job did not start")
	}
	f.DepartureGate, f.ObservedAt = "C3", now
	if err := store.Observe(ctx, f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second:
	case <-ctx.Done():
		t.Fatal("second job blocked by slow first send")
	}
}
