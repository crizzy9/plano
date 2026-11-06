package core

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
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

func dispatcherFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "dispatcher.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	token, err := store.CreateInstallation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	flight := feeds.Flight{ID: "dispatcher-flight", Number: "AA2", Origin: "JFK", Destination: "SFO",
		ObservedAt: time.Now(), Status: "scheduled"}
	if err := store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return store, owner, flight.ID
}

func enqueueDispatcherAlerts(t *testing.T, store *Store, owner, flight, platform string, now, expiry time.Time, count int) {
	t.Helper()
	ctx := t.Context()
	if err := store.Register(ctx, owner, Device{Platform: platform, Kind: "alert", Token: platform + "-fixture-device"}, now); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	message := push.Message{Kind: push.Alert, FlightID: flight, Title: "Gate changed", Body: "AA2 · A3", Timestamp: now, ExpiresAt: expiry}
	for range count {
		if err := enqueue(ctx, tx, flight, message, `SELECT id FROM devices WHERE owner=? AND platform=?`, owner, platform); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.commitOutbox(tx); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherReservesCapacityForTheOtherProvider(t *testing.T) {
	for _, slowPlatform := range []string{"ios", "android"} {
		t.Run(slowPlatform, func(t *testing.T) {
			store, owner, flight := dispatcherFixture(t)
			fastPlatform := "android"
			if slowPlatform == "android" {
				fastPlatform = "ios"
			}
			now := time.Now()
			expiry := now.Add(time.Hour)
			enqueueDispatcherAlerts(t, store, owner, flight, slowPlatform, now, expiry, 16)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			slow, fast, done := make(chan struct{}, 16), make(chan struct{}, 1), make(chan struct{})
			sender := pushFunc(func(ctx context.Context, target push.Target, _ push.Message) error {
				if target.Platform == slowPlatform {
					slow <- struct{}{}
					<-ctx.Done()
					return ctx.Err()
				}
				fast <- struct{}{}
				return nil
			})
			go func() {
				defer close(done)
				store.RunDispatcher(ctx, sender, slog.New(slog.DiscardHandler))
			}()
			defer func() { cancel(); <-done }()
			for range 8 {
				select {
				case <-slow:
				case <-ctx.Done():
					t.Fatal("slow provider did not fill its lane")
				}
			}
			enqueueDispatcherAlerts(t, store, owner, flight, fastPlatform, now, expiry, 1)
			select {
			case <-fast:
			case <-ctx.Done():
				t.Fatal("ready send waited behind a saturated provider")
			}
			var leased int
			if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox o JOIN devices d ON d.id=o.device_id
WHERE d.platform=? AND o.lease_until>0`, slowPlatform).Scan(&leased); err != nil || leased != 8 {
				t.Fatalf("slow lane leases = %d, error = %v", leased, err)
			}
		})
	}
}

func TestDispatcherSkipsClaimedAlertsAfterRevocationOrExpiry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		claimAge time.Duration
		lifetime time.Duration
		revoke   bool
		replace  bool
	}{
		{name: "revoked", lifetime: time.Hour, revoke: true},
		{name: "replaced lease", lifetime: time.Hour, replace: true},
		{name: "expired message", claimAge: time.Second, lifetime: time.Millisecond},
		{name: "expired lease", claimAge: time.Minute, lifetime: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, owner, flight := dispatcherFixture(t)
			claimAt := time.Now().Add(-tc.claimAge)
			enqueueDispatcherAlerts(t, store, owner, flight, "ios", claimAt, claimAt.Add(tc.lifetime), 1)
			jobs, err := store.claim(t.Context(), claimAt, 1)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("claimed %d jobs, error = %v", len(jobs), err)
			}
			if tc.revoke {
				if err := store.RemoveDevices(t.Context(), owner); err != nil {
					t.Fatal(err)
				}
			}
			if tc.replace {
				if _, err := store.db.ExecContext(t.Context(), `UPDATE outbox SET lease_until=? WHERE id=?`, jobs[0].lease+1, jobs[0].id); err != nil {
					t.Fatal(err)
				}
			}
			sender := pushFunc(func(context.Context, push.Target, push.Message) error {
				t.Fatal("sent a push without a current, unexpired lease")
				return nil
			})
			store.deliver(t.Context(), sender, slog.New(slog.DiscardHandler), jobs[0])
		})
	}
}

func TestDispatcherTransportDeadlineHonorsLeaseAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		claimAge   time.Duration
		lifetime   time.Duration
		leaseFirst bool
	}{
		{name: "message expiry", lifetime: 5 * time.Second},
		{name: "lease expiry", claimAge: 25 * time.Second, lifetime: time.Hour, leaseFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, owner, flight := dispatcherFixture(t)
			claimAt := time.Now().Add(-tc.claimAge)
			expiry := claimAt.Add(tc.lifetime)
			enqueueDispatcherAlerts(t, store, owner, flight, "ios", claimAt, expiry, 1)
			jobs, err := store.claim(t.Context(), claimAt, 1)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("claimed %d jobs, error = %v", len(jobs), err)
			}
			want := expiry
			if tc.leaseFirst {
				want = time.Unix(0, jobs[0].lease)
			}
			called := false
			sender := pushFunc(func(ctx context.Context, _ push.Target, _ push.Message) error {
				called = true
				if got, ok := ctx.Deadline(); !ok || !got.Equal(want) {
					t.Fatalf("transport deadline = %s, want %s", got, want)
				}
				return nil
			})
			store.deliver(t.Context(), sender, slog.New(slog.DiscardHandler), jobs[0])
			if !called {
				t.Fatal("eligible job did not reach transport")
			}
		})
	}
}

func TestDispatcherPlatformsUseOnlyConfiguredProviders(t *testing.T) {
	for _, tc := range []struct {
		sender PushSender
		want   []string
	}{
		{sender: push.Sender{}, want: nil},
		{sender: push.Sender{APNs: &push.APNs{}}, want: []string{"ios"}},
		{sender: &push.Sender{FCM: &push.FCM{}}, want: []string{"android"}},
		{sender: push.Sender{APNs: &push.APNs{}, FCM: &push.FCM{}}, want: []string{"ios", "android"}},
	} {
		if got := dispatcherPlatforms(tc.sender); !slices.Equal(got, tc.want) {
			t.Errorf("platforms = %v, want %v", got, tc.want)
		}
	}
}
