package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

type aeroSubscriptionFixture struct {
	alerts     map[int64]feeds.AeroSubscription
	creates    int
	checks     int
	lists      int
	inspects   []int64
	deletes    []int64
	inspectErr int64
	deleteErr  int64
	ambiguous  bool
	rejectNext bool
	truncated  bool
	configured bool
}

func (f *aeroSubscriptionFixture) AeroAlertEndpointConfigured(context.Context) (bool, error) {
	f.checks++
	return f.configured, nil
}

func (f *aeroSubscriptionFixture) ListAeroSubscriptions(context.Context, string) ([]feeds.AeroSubscription, string, error) {
	f.lists++
	rows := make([]feeds.AeroSubscription, 0, len(f.alerts))
	for _, alert := range f.alerts {
		rows = append(rows, alert)
	}
	if f.truncated {
		return rows, "fixture-next", nil
	}
	return rows, "", nil
}

func (f *aeroSubscriptionFixture) InspectAeroSubscription(_ context.Context, id int64) (feeds.AeroSubscription, error) {
	f.inspects = append(f.inspects, id)
	if id == f.inspectErr {
		return feeds.AeroSubscription{}, feeds.ErrUnavailable
	}
	if alert, ok := f.alerts[id]; ok {
		return alert, nil
	}
	return feeds.AeroSubscription{}, feeds.ErrNotFound
}

func (f *aeroSubscriptionFixture) CreateAeroSubscription(_ context.Context, scope feeds.AeroAlertScope, target string) (int64, error) {
	f.creates++
	if f.rejectNext {
		f.rejectNext = false
		return 0, feeds.ErrInvalidQuery
	}
	id := int64(100 + f.creates)
	f.alerts[id] = feeds.AeroSubscription{ID: id, Ident: scope.Ident, Origin: scope.Origin, Destination: scope.Destination,
		Start: scope.Date, End: scope.Date, TargetURL: target}
	if f.ambiguous {
		return 0, feeds.ErrUnavailable
	}
	return id, nil
}

func (f *aeroSubscriptionFixture) DeleteAeroSubscription(_ context.Context, id int64) error {
	f.deletes = append(f.deletes, id)
	if id == f.deleteErr {
		return feeds.ErrUnavailable
	}
	delete(f.alerts, id)
	return nil
}

func fixtureAeroSubscriptionFlight(t *testing.T, s *Store, now time.Time, hours time.Duration, id, number string) feeds.Flight {
	t.Helper()
	f := feeds.Flight{ID: id, Source: "flightaware_aeroapi", ICAONumber: number, Number: number, OriginICAO: "KSFO", DestinationICAO: "KJFK", Origin: "SFO", Destination: "JFK",
		Status: "scheduled", OriginTimeZone: "America/Los_Angeles", ScheduledDeparture: new(now.Add(hours)), ScheduledArrival: new(now.Add(hours + 5*time.Hour)), ObservedAt: now}
	if err := s.Observe(t.Context(), f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return f
}

func fixtureSubscriptionOptions() AeroSubscriptionOptions {
	return AeroSubscriptionOptions{CallbackURL: "https://plano.example.com/v1/webhooks/aeroapi?token=" + strings.Repeat("T", 40), MaxActive: 3, Interval: time.Minute}
}

func TestAeroSubscriptionReconcileScopesAndRetiresOnlyOwnedAlerts(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "subscriptions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner, other := friendInstallation(t, s), friendInstallation(t, s)
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	future := fixtureAeroSubscriptionFlight(t, s, now, 24*time.Hour, "UAL7-fixture", "UAL7")
	sameDay := fixtureAeroSubscriptionFlight(t, s, now, time.Hour, "UAL8-fixture", "UAL8")
	for _, item := range []struct{ owner, flight string }{{owner, future.ID}, {other, future.ID}, {owner, sameDay.ID}} {
		if err := s.Track(ctx, item.owner, item.flight, ""); err != nil {
			t.Fatal(err)
		}
	}
	fixture := &aeroSubscriptionFixture{configured: true, alerts: map[int64]feeds.AeroSubscription{9: {
		ID: 9, Ident: "UAL7", Origin: "KSFO", Destination: "KJFK", Start: "2026-11-06", End: "2026-11-06", TargetURL: "https://existing.example.com/alerts",
	}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := fixtureSubscriptionOptions()
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, step := range []time.Duration{0, time.Minute} {
		if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(step), logger); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.creates != 1 || fixture.checks != 1 || fixture.alerts[101].Start != "2026-11-06" || !strings.Contains(fixture.alerts[101].TargetURL, "plano_alert=") {
		t.Fatalf("registration did not deduplicate route scopes, exclude same-day, or mark ownership: creates=%d checks=%d", fixture.creates, fixture.checks)
	}
	if err := s.Untrack(ctx, owner, future.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(2*time.Minute), logger); err != nil || len(fixture.deletes) != 0 {
		t.Fatalf("alert removed while another installation still tracks the scope: %v", err)
	}
	if err := s.Untrack(ctx, other, future.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(3*time.Minute), logger); err != nil {
		t.Fatal(err)
	}
	if len(fixture.deletes) != 1 || fixture.deletes[0] != 101 || len(fixture.inspects) != 1 || fixture.inspects[0] != 101 || fixture.alerts[9].ID != 9 {
		t.Fatalf("ownership retirement inspected %v and deleted %v", fixture.inspects, fixture.deletes)
	}
}

func TestAeroSubscriptionAmbiguityRecoveryDoesNotRepeatPost(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "subscriptions.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner := friendInstallation(t, s)
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	flight := fixtureAeroSubscriptionFlight(t, s, now, 24*time.Hour, "UAL7-fixture", "UAL7")
	if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	fixture := &aeroSubscriptionFixture{configured: true, ambiguous: true, alerts: make(map[int64]feeds.AeroSubscription)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := fixtureSubscriptionOptions()
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now, logger); err == nil {
		t.Fatal("ambiguous upstream mutation was not reported")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(time.Minute), logger); err != nil || fixture.creates != 1 || fixture.lists != 0 {
		t.Fatalf("restart repeated upstream writes or ignored inspection delay: creates=%d lists=%d error=%v", fixture.creates, fixture.lists, err)
	}
	fixture.truncated = true
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(aeroSubscriptionRetry), logger); err != nil {
		t.Fatal(err)
	}
	states, err := s.managedAeroSubscriptions(ctx)
	if err != nil || len(states) != 1 || states[0].remoteID.Valid || fixture.creates != 1 || fixture.lists != aeroSubscriptionPages {
		t.Fatalf("incomplete inventory claimed a remote ID: %v", err)
	}
	fixture.truncated = false
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(2*aeroSubscriptionRetry), logger); err != nil {
		t.Fatal(err)
	}
	states, err = s.managedAeroSubscriptions(ctx)
	if err != nil || len(states) != 1 || !states[0].remoteID.Valid || states[0].remoteID.Int64 != 101 || fixture.creates != 1 {
		t.Fatalf("complete exact inventory did not recover the remote ID: %v", err)
	}
	if err := s.Untrack(ctx, owner, flight.ID); err != nil {
		t.Fatal(err)
	}
	remote := fixture.alerts[101]
	remote.TargetURL = "https://existing.example.com/alerts"
	fixture.alerts[101] = remote
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(2*aeroSubscriptionRetry+time.Minute), logger); err != nil {
		t.Fatal(err)
	}
	if len(fixture.deletes) != 0 || fixture.alerts[101].ID != 101 {
		t.Fatal("deleted a remote record after ownership could no longer be established")
	}
}

func TestAeroSubscriptionCreateBudgetAndEndpointPrerequisite(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "subscriptions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner := friendInstallation(t, s)
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	for _, number := range []string{"UAL1", "UAL2", "UAL3", "UAL4"} {
		flight := fixtureAeroSubscriptionFlight(t, s, now, 24*time.Hour, number+"-fixture", number)
		if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	fixture := &aeroSubscriptionFixture{alerts: make(map[int64]feeds.AeroSubscription)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := fixtureSubscriptionOptions()
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now, logger); err != nil || fixture.creates != 0 || fixture.checks != 1 {
		t.Fatalf("missing account default did not stop writes: %v", err)
	}
	fixture.configured = true
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(time.Minute), logger); err != nil || fixture.creates != 2 {
		t.Fatalf("per-cycle create budget: creates=%d err=%v", fixture.creates, err)
	}
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(2*time.Minute), logger); err != nil || fixture.creates != 3 {
		t.Fatalf("managed active scope cap: creates=%d err=%v", fixture.creates, err)
	}
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(3*time.Minute), logger); err != nil || fixture.creates != 3 || len(fixture.deletes) != 0 {
		t.Fatalf("cap caused unexpected side effects: %v", err)
	}
}

func TestAeroSubscriptionOptionsRejectInsecureTargets(t *testing.T) {
	for _, raw := range []string{
		"http://plano.example.com/v1/webhooks/aeroapi?token=" + strings.Repeat("T", 40),
		"https://plano.example.com/v1/webhooks/aeroapi?token=short",
		"https://plano.example.com/v1/webhooks/aeroapi?plano_alert=foreign&token=" + strings.Repeat("T", 40),
	} {
		opts := fixtureSubscriptionOptions()
		opts.CallbackURL = raw
		if err := opts.Validate(); err == nil || errors.Is(err, feeds.ErrUnavailable) {
			t.Fatal("unsupported callback target accepted or misclassified")
		}
	}
}

func TestAeroSubscriptionFailedRetirementDoesNotBlockOtherScopes(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "subscriptions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner := friendInstallation(t, s)
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	var tracked []string
	for _, number := range []string{"UAL1", "UAL2", "UAL3"} {
		flight := fixtureAeroSubscriptionFlight(t, s, now, 24*time.Hour, number+"-fixture", number)
		if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
		tracked = append(tracked, flight.ID)
	}
	fixture := &aeroSubscriptionFixture{configured: true, alerts: make(map[int64]feeds.AeroSubscription)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := fixtureSubscriptionOptions()
	for _, step := range []time.Duration{0, time.Minute} {
		if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(step), logger); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.creates != 3 {
		t.Fatalf("fixture creates = %d", fixture.creates)
	}
	for _, id := range tracked {
		if err := s.Untrack(ctx, owner, id); err != nil {
			t.Fatal(err)
		}
	}
	flight := fixtureAeroSubscriptionFlight(t, s, now, 24*time.Hour, "UAL4-fixture", "UAL4")
	if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	fixture.inspectErr, fixture.deleteErr = 101, 102
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(2*time.Minute), logger); err != nil {
		t.Fatal(err)
	}
	if fixture.alerts[101].ID != 101 || fixture.alerts[102].ID != 102 || fixture.alerts[103].ID != 0 || fixture.creates != 4 || len(fixture.inspects) != 3 || len(fixture.deletes) != 2 {
		t.Fatalf("failed scopes blocked independent work: creates=%d inspections=%v deletions=%v", fixture.creates, fixture.inspects, fixture.deletes)
	}
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(3*time.Minute), logger); err != nil || len(fixture.inspects) != 3 || len(fixture.deletes) != 2 {
		t.Fatalf("failed scopes did not back off: %v", err)
	}
}

func TestAeroSubscriptionRejectedScopeDoesNotConsumeActiveQuota(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "subscriptions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner := friendInstallation(t, s)
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	for _, number := range []string{"UAL1", "UAL2"} {
		flight := fixtureAeroSubscriptionFlight(t, s, now, 24*time.Hour, number+"-fixture", number)
		if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	fixture := &aeroSubscriptionFixture{configured: true, rejectNext: true, alerts: make(map[int64]feeds.AeroSubscription)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := fixtureSubscriptionOptions()
	opts.MaxActive = 1
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now, logger); err == nil || fixture.creates != 1 || len(fixture.alerts) != 0 {
		t.Fatalf("provider rejection did not stop first cycle: %v", err)
	}
	if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, now.Add(aeroSubscriptionRetry), logger); err != nil || fixture.creates != 2 || fixture.lists != 0 || fixture.alerts[102].Ident != "UAL2" {
		t.Fatalf("rejection was retried or consumed active capacity: creates=%d lists=%d error=%v", fixture.creates, fixture.lists, err)
	}
}
