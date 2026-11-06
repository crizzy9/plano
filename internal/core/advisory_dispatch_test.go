package core

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestDispatcherDropsSupersededAdvisoryAlerts(t *testing.T) {
	for _, claimBeforeRevision := range []bool{false, true} {
		name := "before claim"
		if claimBeforeRevision {
			name = "after claim"
		}
		t.Run(name, func(t *testing.T) {
			store, owner, flight := dispatcherFixture(t)
			ctx := t.Context()
			now := time.Now().UTC()
			for _, platform := range []string{"ios", "android"} {
				enqueueDispatcherAlerts(t, store, owner, flight, platform, now, now.Add(time.Hour), 1)
			}
			advisory := dispatchAdvisoryFixture(now)
			publishDispatchAdvisory(t, store, advisory, now)
			enqueueDispatchAdvisory(t, store, owner, flight, advisory, now)
			withdrawal := advisory
			withdrawal.ObservedAt = now.Add(-time.Second)
			withdrawal.Kind = "cancelled"
			if !claimBeforeRevision {
				publishDispatchAdvisory(t, store, withdrawal, now)
			}
			var jobs []deliveryJob
			for _, platform := range []string{"ios", "android"} {
				claimed, err := store.claimForPlatform(ctx, now, 2, platform)
				if err != nil || len(claimed) != 2 {
					t.Fatalf("%s claims = %d, error = %v", platform, len(claimed), err)
				}
				jobs = append(jobs, claimed...)
			}
			if claimBeforeRevision {
				publishDispatchAdvisory(t, store, withdrawal, now)
			}
			sends := make(map[string]int)
			sender := pushFunc(func(_ context.Context, target push.Target, msg push.Message) error {
				if msg.Title != "Gate changed" {
					t.Fatalf("superseded warning reached %s transport: %s", target.Platform, msg.Title)
				}
				sends[target.Platform]++
				return nil
			})
			logger := slog.New(slog.DiscardHandler)
			for _, job := range jobs {
				store.deliver(ctx, sender, logger, job)
			}
			if sends["ios"] != 1 || sends["android"] != 1 {
				t.Fatalf("unrelated sends = %v", sends)
			}
			var obsolete, evidence, devices int
			err := store.db.QueryRowContext(ctx, `SELECT
(SELECT COUNT(*) FROM outbox WHERE json_extract(message,'$.Title')='Advisory warning'),
(SELECT COUNT(*) FROM outbox_advisory_evidence),
(SELECT COUNT(*) FROM devices WHERE owner=?)`, owner).Scan(&obsolete, &evidence, &devices)
			if err != nil || obsolete != 0 || evidence != 0 || devices != 2 {
				t.Fatalf("obsolete jobs = %d, evidence rows = %d, devices = %d, error = %v", obsolete, evidence, devices, err)
			}
		})
	}
}

func TestDispatcherChecksDistinctAdvisorySupersessionAfterClaim(t *testing.T) {
	for _, tc := range []struct {
		name, source, airport string
		polled, superseded    bool
	}{
		{name: "signed cancellation", source: feeds.FAASWIM, airport: "KJFK", superseded: true},
		{name: "polled cancellation", source: feeds.FAASWIM, airport: "KJFK", polled: true, superseded: true},
		{name: "different airport", source: feeds.FAASWIM, airport: "KSFO"},
		{name: "different source", source: feeds.Eurocontrol, airport: "KJFK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, owner, flight := dispatcherFixture(t)
			ctx, now := t.Context(), time.Now().UTC()
			for _, platform := range []string{"ios", "android"} {
				if err := store.Register(ctx, owner, Device{Platform: platform, Kind: "alert", Token: platform + "-fixture-device"}, now); err != nil {
					t.Fatal(err)
				}
			}
			advisory := dispatchAdvisoryFixture(now)
			advisory.ID, advisory.Kind, advisory.Airport = "KJFK/A0101/26", "airport_closure", "KJFK"
			if err := store.cachePolledFlightNotices(ctx, advisory.Source, []feeds.Advisory{advisory}, now); err != nil {
				t.Fatal(err)
			}
			enqueueDispatchAdvisory(t, store, owner, flight, advisory, now)
			jobs, err := store.claim(ctx, now, 2)
			if err != nil || len(jobs) != 2 {
				t.Fatalf("claims = %d, error = %v", len(jobs), err)
			}
			cancellation := feeds.Advisory{ID: "KJFK/A0102/26", Source: tc.source, Airport: tc.airport,
				Kind: "notam_cancellation", SupersedesID: advisory.ID, ObservedAt: now}
			if tc.polled {
				if err := store.cachePolledFlightNotices(ctx, tc.source, []feeds.Advisory{cancellation}, now); err != nil {
					t.Fatal(err)
				}
			} else {
				publishDispatchAdvisory(t, store, cancellation, now)
			}
			// Leave the leased evidence as the only record of the old airport.
			if err := store.cachePolledFlightNotices(ctx, advisory.Source, nil, now); err != nil {
				t.Fatal(err)
			}
			sends := make(map[string]int)
			sender := pushFunc(func(_ context.Context, target push.Target, msg push.Message) error {
				if msg.Title != "Advisory warning" {
					t.Fatalf("unexpected %s message: %s", target.Platform, msg.Title)
				}
				sends[target.Platform]++
				return nil
			})
			for _, job := range jobs {
				store.deliver(ctx, sender, slog.New(slog.DiscardHandler), job)
			}
			want := 1
			if tc.superseded {
				want = 0
			}
			if sends["ios"] != want || sends["android"] != want {
				t.Fatalf("dispatch sends = %v, want %d per platform", sends, want)
			}
			if tc.superseded {
				var scopes int
				if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox_advisory_scopes`).Scan(&scopes); err != nil || scopes != 0 {
					t.Fatalf("superseded job scopes = %d, error = %v", scopes, err)
				}
			}
		})
	}
}

func TestDispatcherKeepsCurrentAdvisoryRetries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revise func(*feeds.Advisory, time.Time)
	}{
		{name: "same material evidence", revise: func(a *feeds.Advisory, now time.Time) {
			a.ObservedAt = now.Add(-time.Second)
		}},
		{name: "future withdrawal", revise: func(a *feeds.Advisory, now time.Time) {
			a.ObservedAt, a.Kind = now.Add(time.Hour), "cancelled"
		}},
		{name: "different advisory", revise: func(a *feeds.Advisory, now time.Time) {
			a.ObservedAt, a.ID, a.Kind = now.Add(-time.Second), "another-program", "cancelled"
		}},
		{name: "different source", revise: func(a *feeds.Advisory, now time.Time) {
			a.ObservedAt, a.Source, a.Kind = now.Add(-time.Second), feeds.Eurocontrol, "cancelled"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, owner, flight := dispatcherFixture(t)
			ctx := t.Context()
			now := time.Now().UTC()
			if err := store.Register(ctx, owner, Device{Platform: "ios", Kind: "alert", Token: "fixture-device"}, now); err != nil {
				t.Fatal(err)
			}
			advisory := dispatchAdvisoryFixture(now)
			publishDispatchAdvisory(t, store, advisory, now)
			enqueueDispatchAdvisory(t, store, owner, flight, advisory, now)
			jobs, err := store.claim(ctx, now, 1)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("initial claims = %d, error = %v", len(jobs), err)
			}
			if err := store.resolve(ctx, jobs[0], &push.DeliveryError{Retryable: true}, now); err != nil {
				t.Fatal(err)
			}
			revision := advisory
			tc.revise(&revision, now)
			publishDispatchAdvisory(t, store, revision, now)
			retry, err := store.claim(ctx, now.Add(time.Second), 1)
			if err != nil || len(retry) != 1 || retry[0].id != jobs[0].id || retry[0].attempts != 1 {
				t.Fatalf("retry jobs = %+v, error = %v", retry, err)
			}
			sends := 0
			sender := pushFunc(func(_ context.Context, _ push.Target, msg push.Message) error {
				if msg.Title != "Advisory warning" {
					t.Fatalf("unexpected message: %s", msg.Title)
				}
				sends++
				return nil
			})
			store.deliver(ctx, sender, slog.New(slog.DiscardHandler), retry[0])
			if sends != 1 {
				t.Fatalf("current warning sends = %d", sends)
			}
		})
	}
}

func dispatchAdvisoryFixture(now time.Time) feeds.Advisory {
	return feeds.Advisory{ID: "dispatch-program", Source: feeds.FAASWIM, Airport: "JFK", Kind: "ground_stop",
		Direction: "arrival", Reason: "traffic", ObservedAt: now.Add(-time.Minute), EndsAt: new(now.Add(time.Hour))}
}

func publishDispatchAdvisory(t *testing.T, store *Store, advisory feeds.Advisory, now time.Time) {
	t.Helper()
	if err := store.PublishAdvisory(t.Context(), advisory, now); err != nil {
		t.Fatal(err)
	}
}

func enqueueDispatchAdvisory(t *testing.T, store *Store, owner, flight string, advisory feeds.Advisory, now time.Time) {
	t.Helper()
	ctx := t.Context()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	message := push.Message{Kind: push.Alert, FlightID: flight, Title: "Advisory warning", Body: advisory.Reason,
		Timestamp: now, ExpiresAt: now.Add(30 * time.Minute)}
	if err := enqueueWithAdvisories(ctx, tx, flight, message, []forecastAdvisory{advisoryEvidence(advisory)},
		`SELECT id FROM devices WHERE owner=? AND kind='alert'`, owner); err != nil {
		t.Fatal(err)
	}
	if err := store.commitOutbox(tx); err != nil {
		t.Fatal(err)
	}
}
