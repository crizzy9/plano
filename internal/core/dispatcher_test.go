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

type friendGuardFixture struct {
	pushFunc
	beforeSubmit func(context.Context)
}

func (s friendGuardFixture) SendAuthorized(ctx context.Context, target push.Target, message push.Message, authorize func(context.Context) error) error {
	s.beforeSubmit(ctx)
	if err := authorize(ctx); err != nil {
		return err
	}
	return s.Send(ctx, target, message)
}

func TestDispatcherChecksFriendAuthorizationAtProviderSubmission(t *testing.T) {
	store, sharer, id := dispatcherFixture(t)
	ctx := t.Context()
	viewer := friendInstallation(t, store)
	if err := store.Track(ctx, sharer, id, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, viewer, Device{Platform: "android", Kind: "alert", Token: "friend-device"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	invite, err := store.CreateFriendInvite(ctx, sharer, "Sharer", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	friend, err := store.AcceptFriendInvite(ctx, viewer, invite.Code, "Viewer", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.GrantFriendFlight(ctx, sharer, friend.ID, id); err != nil {
		t.Fatal(err)
	}
	if err := store.SetFriendAlertMode(ctx, viewer, friend.ID, FriendAlertAll); err != nil {
		t.Fatal(err)
	}
	flight, err := store.Flight(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	flight.DepartureGate = "B12"
	observeFlightAlertFixture(t, store, &flight)
	job := claimFlightAlertJobs(t, store, 1)[0]
	guardCalled := false
	sender := friendGuardFixture{
		pushFunc: func(context.Context, push.Target, push.Message) error {
			t.Fatal("revoked friend alert reached provider transport")
			return nil
		},
		beforeSubmit: func(attempt context.Context) {
			guardCalled = true
			if err := store.RevokeFriendFlight(attempt, sharer, friend.ID, id); err != nil {
				t.Fatal(err)
			}
			if err := store.GrantFriendFlight(attempt, sharer, friend.ID, id); err != nil {
				t.Fatal(err)
			}
		},
	}
	store.deliver(ctx, sender, slog.New(slog.DiscardHandler), job)
	var jobs int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE id=?`, job.id).Scan(&jobs); err != nil || !guardCalled || jobs != 0 {
		t.Fatalf("provider guard reached = %t, retired jobs remaining = %d, error = %v", guardCalled, jobs, err)
	}
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
	f.ArrivalGate, f.ObservedAt = "C3", now
	if err := store.Observe(ctx, f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second:
	case <-ctx.Done():
		t.Fatal("second job blocked by slow first send")
	}
}

func TestDispatcherDefersSameAPNsAlertFamilyWhileSending(t *testing.T) {
	store, owner, id := dispatcherFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	flight, err := store.Flight(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, id, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, owner, Device{Platform: "ios", Kind: "alert", Token: "ios-family-fixture"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	flight.DepartureGate = "B2"
	observeFlightAlertFixture(t, store, &flight)
	jobs, err := store.claimForPlatform(ctx, time.Now().Add(time.Second), 1, "ios")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claimed %d original jobs, error = %v", len(jobs), err)
	}
	original := jobs[0]
	logger := slog.New(slog.DiscardHandler)
	started := make(chan push.Message, 1)
	release, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		store.deliver(ctx, pushFunc(func(ctx context.Context, _ push.Target, message push.Message) error {
			started <- message
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}), logger, original)
	}()
	defer func() { cancel(); <-finished }()
	var sentOriginal push.Message
	select {
	case sentOriginal = <-started:
	case <-ctx.Done():
		t.Fatal("original alert did not reach transport")
	}
	if sentOriginal.APNsCollapseID == "" || sentOriginal.APNsCollapseID == original.message.CollapseID ||
		sentOriginal.CollapseID != original.message.CollapseID {
		t.Fatalf("APNs family %q, sent event ID %q, want original event ID %q", sentOriginal.APNsCollapseID, sentOriginal.CollapseID, original.message.CollapseID)
	}

	flight.DepartureGate, flight.ArrivalGate = "C3", "D4"
	observeFlightAlertFixture(t, store, &flight)
	jobs, err = store.claimForPlatform(ctx, time.Now().Add(time.Second), 2, "ios")
	if err != nil || len(jobs) != 2 {
		t.Fatalf("claimed %d replacement and independent jobs, error = %v", len(jobs), err)
	}
	var replacement, independent deliveryJob
	for _, job := range jobs {
		switch job.message.Body {
		case "AA2 · C3":
			replacement = job
		case "AA2 · D4":
			independent = job
		}
	}
	if replacement.id == 0 || independent.id == 0 {
		t.Fatal("missing departure or arrival gate change")
	}
	var sent []push.Message
	sender := pushFunc(func(_ context.Context, _ push.Target, message push.Message) error {
		sent = append(sent, message)
		return nil
	})
	store.deliver(ctx, sender, logger, replacement)
	store.deliver(ctx, sender, logger, independent)
	if len(sent) != 1 || sent[0].Body != independent.message.Body || sent[0].APNsCollapseID == "" ||
		sent[0].APNsCollapseID == sentOriginal.APNsCollapseID {
		t.Fatalf("APNs sent while original alert was in flight = %+v, want independent arrival gate alert", sent)
	}
	var due, lease int64
	var attempts, done int
	var eventID string
	if err := store.db.QueryRowContext(ctx, `SELECT due,lease_until,attempts,done,json_extract(message,'$.CollapseID') FROM outbox WHERE id=?`, replacement.id).
		Scan(&due, &lease, &attempts, &done, &eventID); err != nil {
		t.Fatal(err)
	}
	if due <= replacement.message.Timestamp.UnixNano() || lease != 0 || attempts != 0 || done != 0 {
		t.Fatalf("replacement due=%d lease=%d attempts=%d done=%d, want deferred and unleased", due, lease, attempts, done)
	}
	if eventID != replacement.message.CollapseID || eventID == original.message.CollapseID || eventID == sentOriginal.APNsCollapseID {
		t.Fatalf("replacement persisted event ID %q, want its own event identity", eventID)
	}

	close(release)
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("original alert did not finish")
	}
	jobs, err = store.claimForPlatform(ctx, time.Unix(0, due), 1, "ios")
	if err != nil || len(jobs) != 1 || jobs[0].id != replacement.id {
		t.Fatalf("claimed replacement jobs = %+v, error = %v", jobs, err)
	}
	store.deliver(ctx, sender, logger, jobs[0])
	if len(sent) != 2 || sent[1].Body != replacement.message.Body || sent[1].APNsCollapseID != sentOriginal.APNsCollapseID ||
		sent[1].CollapseID != eventID {
		t.Fatalf("APNs sends = %+v, want deferred departure gate alert under the original family ID", sent)
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
