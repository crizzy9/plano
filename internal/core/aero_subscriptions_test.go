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

func TestAeroSubscriptionPlannerSelectsEligibleDirectInbound(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "subscriptions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	onward := fixtureAeroSubscriptionFlight(t, s, now, 30*time.Hour, "UAL7-onward", "UAL7")
	inbound := feeds.Flight{ID: "UAL6-inbound", ICAONumber: "UAL6", Source: onward.Source, Status: "scheduled", TailNumber: "N123AA",
		Origin: "LAX", OriginICAO: "KLAX", OriginTimeZone: onward.OriginTimeZone, Destination: "SFO", DestinationICAO: "KSFO",
		ScheduledDeparture: new(now.Add(24 * time.Hour)), ScheduledArrival: new(now.Add(26 * time.Hour)), ObservedAt: now}
	onward.InboundID, onward.TailNumber = inbound.ID, inbound.TailNumber
	if err := s.Track(ctx, friendInstallation(t, s), onward.ID, ""); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*feeds.Flight, *feeds.Flight)
		want   bool
	}{
		{name: "scheduled", want: true},
		{name: "airborne inbound remains wanted with an elapsed ETA", want: true, mutate: func(parent, leg *feeds.Flight) {
			parent.ScheduledDeparture = new(now.Add(2 * time.Hour))
			leg.Status = "airborne"
			leg.ScheduledDeparture, leg.ActualTakeoff = new(now.Add(-2*time.Hour)), new(now.Add(-90*time.Minute))
			leg.ScheduledArrival = new(now.Add(time.Hour))
			leg.EstimatedArrival = new(now.Add(-95 * time.Minute))
		}},
		{name: "landed inbound remains wanted", want: true, mutate: func(parent, leg *feeds.Flight) {
			parent.ScheduledDeparture = new(now.Add(2 * time.Hour))
			leg.Status = "landed"
			leg.ScheduledDeparture, leg.ScheduledArrival = new(now.Add(-3*time.Hour)), new(now.Add(-time.Hour))
			leg.ActualLanding = new(now.Add(-time.Minute))
		}},
		{name: "different first leg", mutate: func(_, leg *feeds.Flight) { leg.ID = "different" }},
		{name: "no explicit link", mutate: func(parent, _ *feeds.Flight) { parent.InboundID = "" }},
		{name: "different airport", mutate: func(_, leg *feeds.Flight) { leg.Destination, leg.DestinationICAO = "BOS", "KBOS" }},
		{name: "different aircraft", mutate: func(_, leg *feeds.Flight) { leg.TailNumber = "N456BB" }},
		{name: "cancelled", mutate: func(_, leg *feeds.Flight) { leg.Status = "cancelled" }},
		{name: "diverted", mutate: func(_, leg *feeds.Flight) { leg.Status = "diverted" }},
		{name: "unknown", mutate: func(_, leg *feeds.Flight) { leg.Status = "unknown" }},
		{name: "recorded gate arrival", mutate: func(_, leg *feeds.Flight) { leg.ActualArrival = new(now) }},
		{name: "invalid route scope", mutate: func(_, leg *feeds.Flight) { leg.OriginTimeZone = "" }},
		{name: "inconsistent schedule", mutate: func(parent, leg *feeds.Flight) { leg.ScheduledDeparture = parent.ScheduledDeparture }},
		{name: "scheduled arrival before departure", mutate: func(_, leg *feeds.Flight) {
			leg.ScheduledArrival = new(leg.ScheduledDeparture.Add(-time.Minute))
		}},
		{name: "confirmed landing before takeoff", mutate: func(_, leg *feeds.Flight) {
			leg.ActualTakeoff, leg.ActualLanding = new(now.Add(-time.Minute)), new(now.Add(-time.Hour))
		}},
		{name: "onward departed", mutate: func(parent, _ *feeds.Flight) { parent.ActualDeparture = new(now) }},
		{name: "onward outside active window", mutate: func(parent, _ *feeds.Flight) { parent.ScheduledDeparture = new(now.Add(-7 * time.Hour)) }},
		{name: "parent aliases its own inbound"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent, leg := onward, inbound
			parent.ObservedAt = now.Add(time.Duration(i+1) * time.Millisecond)
			if tc.mutate != nil {
				tc.mutate(&parent, &leg)
			}
			if err := s.Observe(ctx, parent, nil, []feeds.Flight{leg, inbound}, nil); err != nil {
				t.Fatal(err)
			}
			if tc.name == "parent aliases its own inbound" {
				if _, err := s.db.ExecContext(ctx, `UPDATE aero_instance_keys SET provider_id=? WHERE flight_id=?`, inbound.ID, parent.ID); err != nil {
					t.Fatal(err)
				}
			}
			for _, include := range []bool{false, true} {
				wanted, complete, err := s.wantedAeroSubscriptions(ctx, now, include)
				wantCount := 1
				if include && tc.want {
					wantCount++
				}
				if err != nil || !complete || len(wanted) != wantCount || wanted[0].scope.Ident != onward.ICAONumber ||
					wantCount == 2 && wanted[1].scope.Ident != inbound.ICAONumber {
					t.Fatalf("include inbound %t yielded %+v, complete=%t, err=%v", include, wanted, complete, err)
				}
			}
		})
	}
}

func TestAeroInboundSubscriptionsShareQuotaAndOwnership(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "subscriptions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner := friendInstallation(t, s)
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	inbound := fixtureAeroSubscriptionFlight(t, s, now, 24*time.Hour, "UAL6-inbound", "UAL6")
	inbound.Origin, inbound.OriginICAO, inbound.Destination, inbound.DestinationICAO = "LAX", "KLAX", "SFO", "KSFO"
	setInbound := func(onward *feeds.Flight, leg feeds.Flight) {
		t.Helper()
		onward.InboundID, onward.ObservedAt = leg.ID, onward.ObservedAt.Add(time.Millisecond)
		if err := s.Observe(ctx, *onward, nil, []feeds.Flight{leg}, nil); err != nil {
			t.Fatal(err)
		}
	}
	parents := []feeds.Flight{
		fixtureAeroSubscriptionFlight(t, s, now, 31*time.Hour, "UAL7-onward", "UAL7"),
		fixtureAeroSubscriptionFlight(t, s, now, 31*time.Hour, "UAL7-alias", "UAL7"),
	}
	for i := range parents {
		setInbound(&parents[i], inbound)
		if err := s.Track(ctx, owner, parents[i].ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	fixture := &aeroSubscriptionFixture{configured: true, alerts: map[int64]feeds.AeroSubscription{9: {
		ID: 9, Ident: "UAL6", Origin: "KLAX", Destination: "KSFO", Start: "2026-11-06", End: "2026-11-06", TargetURL: "https://existing.example.com/alerts",
	}}}
	opts := fixtureSubscriptionOptions()
	opts.IncludeInbound, opts.MaxActive = true, 1
	logger := slog.New(slog.DiscardHandler)
	reconcile := func(at time.Time) {
		t.Helper()
		if err := s.reconcileAeroSubscriptions(ctx, fixture, opts, at, logger); err != nil {
			t.Fatal(err)
		}
	}
	reconcile(now)
	if fixture.creates != 1 || fixture.alerts[101].Ident != "UAL7" {
		t.Fatalf("direct tracking did not get first capacity: creates=%d ident=%q", fixture.creates, fixture.alerts[101].Ident)
	}
	opts.MaxActive = 2
	moving := inbound
	moving.Status, moving.ActualTakeoff = "airborne", new(now)
	for i := range parents {
		setInbound(&parents[i], moving)
	}
	reconcile(now.Add(time.Minute))
	if fixture.creates != 1 {
		t.Fatalf("moving inbound created a scope for its original future date: creates=%d", fixture.creates)
	}
	other := inbound
	other.ID = "UAL6-second-instance"
	setInbound(&parents[0], other)
	reconcile(now.Add(2 * time.Minute))
	if fixture.creates != 2 || fixture.alerts[102].Ident != "UAL6" || fixture.alerts[102].Origin != "KLAX" || fixture.alerts[102].Destination != "KSFO" {
		t.Fatalf("inbound did not share capacity or deduplicate: creates=%d ident=%q", fixture.creates, fixture.alerts[102].Ident)
	}
	other.Status, other.ActualTakeoff = "airborne", new(now.Add(time.Minute))
	setInbound(&parents[0], other)
	reconcile(now.Add(3 * time.Minute))
	if fixture.creates != 2 || len(fixture.deletes) != 0 {
		t.Fatalf("movement retired an existing scope: creates=%d deletes=%v", fixture.creates, fixture.deletes)
	}
	inbound.ObservedAt = now.Add(time.Millisecond)
	if err := s.Observe(ctx, inbound, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, inbound.ID, ""); err != nil {
		t.Fatal(err)
	}
	opts.IncludeInbound = false
	reconcile(now.Add(4 * time.Minute))
	if len(fixture.deletes) != 0 || fixture.creates != 2 {
		t.Fatalf("shared scope was replaced or retired: creates=%d deletes=%v", fixture.creates, fixture.deletes)
	}
	if err := s.Untrack(ctx, owner, inbound.ID); err != nil {
		t.Fatal(err)
	}
	reconcile(now.Add(5 * time.Minute))
	if len(fixture.deletes) != 1 || fixture.deletes[0] != 102 || len(fixture.inspects) != 1 || fixture.inspects[0] != 102 || fixture.alerts[9].ID != 9 {
		t.Fatalf("inbound-only retirement inspected %v and deleted %v", fixture.inspects, fixture.deletes)
	}
}

func TestAeroSubscriptionRejectsCurrentUTCDateBeforeLocalMidnight(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "subscriptions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner := friendInstallation(t, s)
	now := time.Date(2026, time.November, 6, 2, 0, 0, 0, time.UTC)
	blocked := fixtureAeroSubscriptionFlight(t, s, now, 12*time.Hour, "UAL6-current-utc", "UAL6")
	eligible := fixtureAeroSubscriptionFlight(t, s, now, 36*time.Hour, "UAL7-future-utc", "UAL7")
	if scope, start, ok := blocked.AeroAlertScope(); !ok || !start.After(now) || scope.Date != now.Format(time.DateOnly) {
		t.Fatalf("fixture requires local midnight after now on the same UTC date: %v, %v", scope, start)
	}
	for _, flight := range []feeds.Flight{blocked, eligible} {
		if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	fixture := &aeroSubscriptionFixture{configured: true, alerts: make(map[int64]feeds.AeroSubscription)}
	if err := s.reconcileAeroSubscriptions(ctx, fixture, fixtureSubscriptionOptions(), now, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if fixture.creates != 1 || fixture.alerts[101].Ident != eligible.ICAONumber {
		t.Fatalf("current-UTC-date scope was posted: creates=%d ident=%q", fixture.creates, fixture.alerts[101].Ident)
	}
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
