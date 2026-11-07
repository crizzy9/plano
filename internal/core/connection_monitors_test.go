package core

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

type monitorFixture struct {
	store        *Store
	path         string
	owner, other string
	in, out      feeds.Flight
	now          time.Time
}

func newMonitorFixture(t *testing.T) *monitorFixture {
	t.Helper()
	ctx := t.Context()
	f := &monitorFixture{path: filepath.Join(t.TempDir(), "connections.db"), now: time.Now().UTC()}
	var err error
	f.store, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	makeOwner := func(token string) string {
		installation, err := f.store.CreateInstallation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := f.store.Installation(ctx, installation)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.Register(ctx, owner, Device{Platform: "ios", Kind: "alert", Token: token}, f.now); err != nil {
			t.Fatal(err)
		}
		return owner
	}
	f.owner, f.other = makeOwner("owner-alert"), makeOwner("other-alert")
	f.in = feeds.Flight{ID: "connection-in", Number: "PL101", Origin: "SFO", Destination: "ORD", Status: "scheduled", ObservedAt: f.now}
	f.out = feeds.Flight{ID: "connection-out", Number: "PL202", Origin: "ORD", Destination: "JFK", Status: "scheduled", ObservedAt: f.now}
	for _, flight := range []feeds.Flight{f.in, f.out} {
		if err := f.store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Track(ctx, f.owner, flight.ID, "12A"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Track(ctx, f.other, f.out.ID, ""); err != nil {
		t.Fatal(err)
	}
	return f
}

func monitorFixtureScore(ctx context.Context, in, out feeds.Flight, now time.Time, gate ConnectionGatePredictor) (ConnectionAssessment, error) {
	// Exercise a real SQLite read through the evaluator's transaction.
	if _, err := gate(ctx, in, true, now); err != nil {
		return ConnectionAssessment{}, err
	}
	risk := out.ProviderStatus
	if risk == "" {
		risk = in.ProviderStatus
	}
	if risk == "" {
		risk = "Relaxed"
	}
	return ConnectionAssessment{Risk: risk, Monitorable: true, MonitorUntil: now.Add(time.Hour), AlertUntil: now.Add(10 * time.Minute)}, nil
}

func (f *monitorFixture) observe(t *testing.T, flight *feeds.Flight, risk string, at time.Time) {
	t.Helper()
	flight.ProviderStatus, flight.ObservedAt = risk, at
	if err := f.store.Observe(t.Context(), *flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func (f *monitorFixture) start(t *testing.T, evaluate ConnectionEvaluator) {
	t.Helper()
	if err := f.store.MonitorConnection(t.Context(), f.owner, f.in.ID, f.out.ID, f.now, evaluate); err != nil {
		t.Fatal(err)
	}
}

func (f *monitorFixture) check(t *testing.T, now time.Time, evaluate ConnectionEvaluator) {
	t.Helper()
	if err := f.store.checkConnectionMonitors(t.Context(), now, evaluate); err != nil {
		t.Fatal(err)
	}
}

func (f *monitorFixture) claim(t *testing.T, now time.Time, count int) []deliveryJob {
	t.Helper()
	jobs, err := f.store.claim(t.Context(), now, 8)
	if err != nil || len(jobs) != count {
		t.Fatalf("claimed %d jobs, want %d, error %v", len(jobs), count, err)
	}
	return jobs
}

func TestConnectionMonitorWorseningIsDurableAndPrivate(t *testing.T) {
	ctx := t.Context()
	f := newMonitorFixture(t)
	if err := f.store.MonitorConnection(ctx, f.other, f.in.ID, f.out.ID, f.now, monitorFixtureScore); !errors.Is(err, ErrNotFound) {
		t.Fatalf("untracked inbound allowed monitoring: %v", err)
	}
	f.start(t, monitorFixtureScore)
	f.claim(t, f.now, 0)
	first := f.now.Add(time.Second)
	f.observe(t, &f.in, "Tight", first)
	f.start(t, monitorFixtureScore) // An idempotent retry must not reset the baseline.
	f.check(t, first, monitorFixtureScore)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.store = reopened
	watches, err := f.store.ConnectionMonitors(ctx, f.owner)
	if err != nil || len(watches) != 1 || watches[0].Risk != "Tight" {
		t.Fatalf("reopened monitors: %v, %v", watches, err)
	}
	if watches, err := f.store.ConnectionMonitors(ctx, f.other); err != nil || len(watches) != 0 {
		t.Fatalf("other installation saw a monitor: %v, %v", watches, err)
	}
	job := f.claim(t, first, 1)[0]
	if job.target.Token != "owner-alert" || job.message.Kind != push.Alert || job.message.FlightID != f.out.ID || !strings.Contains(job.message.Body, "tight") {
		t.Fatalf("unexpected target or alert: %v, %v", job.target.Platform, job.message.Kind)
	}
	second := first.Add(time.Second)
	f.observe(t, &f.out, "Risky", second)
	f.check(t, second, monitorFixtureScore)
	latest := f.claim(t, second, 1)[0]
	if latest.id == job.id || latest.message.CollapseID != job.message.CollapseID || !strings.Contains(latest.message.Body, "risky") {
		t.Fatal("worsening did not supersede the leased warning")
	}
	third := second.Add(time.Second)
	f.observe(t, &f.out, "Relaxed", third)
	f.check(t, third, monitorFixtureScore)
	f.claim(t, third, 0)
	for _, stale := range []deliveryJob{job, latest} {
		f.store.deliver(ctx, noConnectionSend(t), slog.New(slog.DiscardHandler), stale)
	}
	fourth := third.Add(time.Second)
	f.observe(t, &f.out, "Risky", fourth)
	f.check(t, fourth, monitorFixtureScore)
	latest = f.claim(t, fourth, 1)[0]
	if latest.message.CollapseID != job.message.CollapseID {
		t.Fatal("new worsening lost the pair's collapse identity")
	}
	if err := f.store.Untrack(ctx, f.owner, f.in.ID); err != nil {
		t.Fatal(err)
	}
	if active, err := f.store.ConnectionMonitored(ctx, f.owner, f.in.ID, f.out.ID); active || err != nil {
		t.Fatalf("monitor survived untracking inbound: %v, %v", active, err)
	}
	f.store.deliver(ctx, noConnectionSend(t), slog.New(slog.DiscardHandler), latest)
}

func TestConnectionMonitorFencesDirtyLeasesUntilReassessment(t *testing.T) {
	ctx := t.Context()
	f := newMonitorFixture(t)
	f.start(t, monitorFixtureScore)
	first := f.now.Add(time.Second)
	f.observe(t, &f.in, "Tight", first)
	f.check(t, first, monitorFixtureScore)
	job := f.claim(t, first, 1)[0]
	second := first.Add(time.Second)
	f.observe(t, &f.in, "Tight", second)
	f.store.deliver(ctx, noConnectionSend(t), slog.New(slog.DiscardHandler), job)
	f.claim(t, second, 0)
	failed := errors.New("temporary scoring failure")
	fail := func(context.Context, feeds.Flight, feeds.Flight, time.Time, ConnectionGatePredictor) (ConnectionAssessment, error) {
		return ConnectionAssessment{}, failed
	}
	if err := f.store.checkConnectionMonitors(ctx, second, fail); !errors.Is(err, failed) {
		t.Fatalf("evaluation error lost: %v", err)
	}
	retry := second.Add(connectionCheckRetry)
	f.claim(t, retry, 0)
	f.check(t, retry, monitorFixtureScore)
	latest := f.claim(t, retry, 1)[0]
	if latest.id != job.id || latest.attempts != 0 {
		t.Fatal("same risk replaced the warning or consumed a delivery attempt")
	}
	if err := f.store.UnmonitorConnection(ctx, f.owner, f.in.ID, f.out.ID); err != nil {
		t.Fatal(err)
	}
	f.store.deliver(ctx, noConnectionSend(t), slog.New(slog.DiscardHandler), latest)
}

func TestConnectionMonitorRechecksAtProviderSubmission(t *testing.T) {
	for _, stop := range []bool{false, true} {
		name := "new observation"
		if stop {
			name = "stop monitoring"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newMonitorFixture(t)
			f.start(t, monitorFixtureScore)
			first := f.now.Add(time.Second)
			f.observe(t, &f.in, "Tight", first)
			f.check(t, first, monitorFixtureScore)
			job := f.claim(t, first, 1)[0]
			second := first.Add(time.Second)
			prepared := false
			sender := guardedSenderFixture{
				pushFunc: noConnectionSend(t),
				beforeSubmit: func(attempt context.Context) {
					prepared = true
					if stop {
						if err := f.store.UnmonitorConnection(attempt, f.owner, f.in.ID, f.out.ID); err != nil {
							t.Fatal(err)
						}
					} else {
						f.observe(t, &f.in, "Tight", second)
					}
				},
			}
			f.store.deliver(ctx, sender, slog.New(slog.DiscardHandler), job)
			if !prepared {
				t.Fatal("dispatcher did not reach the guarded provider boundary")
			}
			f.claim(t, second, 0)
			f.check(t, second, monitorFixtureScore)
			if stop {
				f.claim(t, second, 0)
				return
			}
			rechecked := f.claim(t, second, 1)[0]
			if rechecked.id != job.id || rechecked.attempts != 0 {
				t.Fatal("waiting for reassessment replaced the warning or consumed a delivery attempt")
			}
		})
	}
}

func TestConnectionMonitorClockWorseningRetiresAfterAlertExpiry(t *testing.T) {
	ctx := t.Context()
	f := newMonitorFixture(t)
	threshold := f.now.Add(5 * time.Second)
	expiry := threshold.Add(time.Minute)
	evaluate := func(_ context.Context, _, _ feeds.Flight, now time.Time, _ ConnectionGatePredictor) (ConnectionAssessment, error) {
		if now.Before(threshold) {
			return ConnectionAssessment{Risk: "Relaxed", Monitorable: true, NextCheck: threshold, MonitorUntil: expiry}, nil
		}
		return ConnectionAssessment{Risk: "Missed", AlertUntil: expiry}, nil
	}
	f.start(t, evaluate)
	f.check(t, threshold.Add(-time.Nanosecond), evaluate)
	f.claim(t, threshold.Add(-time.Nanosecond), 0)
	f.check(t, threshold, evaluate)
	if active, err := f.store.ConnectionMonitored(ctx, f.owner, f.in.ID, f.out.ID); active || err != nil {
		t.Fatalf("terminal pair stayed active: %v, %v", active, err)
	}
	job := f.claim(t, threshold, 1)[0]
	if !job.message.ExpiresAt.Equal(expiry) || !strings.Contains(job.message.Body, "missed") {
		t.Fatal("terminal worsening had unexpected contents or expiry")
	}
	f.check(t, expiry, evaluate)
	var count int
	if err := f.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM connection_monitors`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("terminal record not removed: %d, %v", count, err)
	}
	f.store.deliver(ctx, noConnectionSend(t), slog.New(slog.DiscardHandler), job)
}

func TestConnectionMonitorKeepsBaselineWhileProviderDataIsIncomplete(t *testing.T) {
	ctx := t.Context()
	f := newMonitorFixture(t)
	deadline := f.now.Add(time.Minute)
	evaluate := func(ctx context.Context, in, out feeds.Flight, now time.Time, gate ConnectionGatePredictor) (ConnectionAssessment, error) {
		if out.ProviderStatus == "incomplete" {
			return ConnectionAssessment{Incomplete: true}, nil
		}
		assessment, err := monitorFixtureScore(ctx, in, out, now, gate)
		assessment.MonitorUntil = deadline
		return assessment, err
	}
	f.start(t, evaluate)
	first := f.now.Add(time.Second)
	f.observe(t, &f.out, "incomplete", first)
	f.check(t, first, evaluate)
	monitors, err := f.store.ConnectionMonitors(ctx, f.owner)
	if err != nil || len(monitors) != 1 || monitors[0].Risk != "Relaxed" {
		t.Fatalf("missing fields replaced the baseline: %v, %v", monitors, err)
	}
	f.claim(t, first, 0)
	second := first.Add(time.Second)
	f.observe(t, &f.out, "Risky", second)
	f.check(t, second, evaluate)
	job := f.claim(t, second, 1)[0]
	f.observe(t, &f.out, "incomplete", second.Add(time.Second))
	f.check(t, second.Add(time.Second), evaluate)
	f.store.deliver(ctx, noConnectionSend(t), slog.New(slog.DiscardHandler), job)
	f.check(t, deadline, evaluate)
	if active, err := f.store.ConnectionMonitored(ctx, f.owner, f.in.ID, f.out.ID); active || err != nil {
		t.Fatalf("incomplete monitor exceeded its last known deadline: %v, %v", active, err)
	}
	f.claim(t, deadline, 0)
}

func noConnectionSend(t *testing.T) pushFunc {
	t.Helper()
	return func(context.Context, push.Target, push.Message) error {
		t.Fatal("stale connection warning reached the push transport")
		return nil
	}
}
