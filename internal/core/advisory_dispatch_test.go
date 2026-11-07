package core

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestDispatcherSerializesAPNsAdvisoryRevisions(t *testing.T) {
	store, owner, id := dispatcherFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	flight, err := store.Flight(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	flight.ObservedAt, flight.DestinationICAO, flight.Status = now, "KSFO", "taxi_out"
	flight.ActualDeparture, flight.ScheduledArrival = new(now.Add(-time.Minute)), new(now.Add(35*time.Minute))
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, id, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, owner, Device{Platform: "ios", Kind: "alert", Token: "ios-advisory-fixture"}, now); err != nil {
		t.Fatal(err)
	}
	program := feeds.Advisory{ID: "tfms/gs/KSFO", Source: feeds.FAASWIM, Airport: "KSFO", Kind: "ground_stop",
		Direction: "arrival", AirportContextOnly: true, Reason: "Ground stop due to thunderstorms.",
		ObservedAt: now.Add(-time.Minute), StartsAt: new(now.Add(-10 * time.Minute)), EndsAt: new(now.Add(time.Hour))}
	publishDispatchAdvisory(t, store, program, now)
	jobs, err := store.claimForPlatform(ctx, now, 1, "ios")
	if err != nil || len(jobs) != 1 || jobs[0].message.Body != program.Reason {
		t.Fatalf("original advisory claims = %+v, error = %v", jobs, err)
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
		t.Fatal("original advisory did not reach transport")
	}
	effectiveCollapseID := func(message push.Message) string {
		if message.APNsCollapseID != "" {
			return message.APNsCollapseID
		}
		return message.CollapseID
	}
	family := effectiveCollapseID(sentOriginal)
	if family == "" || sentOriginal.Body != program.Reason {
		t.Fatalf("original advisory transport message = %+v", sentOriginal)
	}

	publishedAt := time.Now().UTC()
	revision := program
	revision.ObservedAt, revision.Reason = publishedAt, "Ground stop extended due to thunderstorms."
	revision.EndsAt = new(now.Add(90 * time.Minute))
	publishDispatchAdvisory(t, store, revision, publishedAt)
	other := revision
	other.ID, other.Kind, other.Reason = "tfms/gdp/KSFO", "ground_delay", "Ground delay due to runway capacity."
	publishDispatchAdvisory(t, store, other, publishedAt)
	jobs, err = store.claimForPlatform(ctx, publishedAt, 2, "ios")
	if err != nil || len(jobs) != 2 {
		t.Fatalf("revised and independent claims = %+v, error = %v", jobs, err)
	}
	var revised, independent deliveryJob
	for _, job := range jobs {
		switch job.message.Body {
		case revision.Reason:
			revised = job
		case other.Reason:
			independent = job
		}
	}
	if revised.id == 0 || independent.id == 0 {
		t.Fatalf("missing revised or independent advisory in %+v", jobs)
	}
	var sent []push.Message
	sender := pushFunc(func(_ context.Context, _ push.Target, message push.Message) error {
		sent = append(sent, message)
		return nil
	})
	store.deliver(ctx, sender, logger, revised)
	store.deliver(ctx, sender, logger, independent)
	if len(sent) != 1 || sent[0].Body != other.Reason || effectiveCollapseID(sent[0]) == family {
		t.Fatalf("advisory sends while original was in flight = %+v, want only the independent alert", sent)
	}
	var due, lease int64
	var attempts, done int
	if err := store.db.QueryRowContext(ctx, `SELECT due,lease_until,attempts,done FROM outbox WHERE id=?`, revised.id).
		Scan(&due, &lease, &attempts, &done); err != nil {
		t.Fatal(err)
	}
	if due <= publishedAt.UnixNano() || lease != 0 || attempts != 0 || done != 0 {
		t.Fatalf("revision due=%d lease=%d attempts=%d done=%d, want deferred and unleased", due, lease, attempts, done)
	}

	close(release)
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("original advisory did not finish")
	}
	jobs, err = store.claimForPlatform(ctx, time.Unix(0, due), 1, "ios")
	if err != nil || len(jobs) != 1 || jobs[0].id != revised.id || jobs[0].attempts != 0 {
		t.Fatalf("reclaimed revision = %+v, error = %v", jobs, err)
	}
	store.deliver(ctx, sender, logger, jobs[0])
	if len(sent) != 2 || sent[1].Body != revision.Reason || effectiveCollapseID(sent[1]) != family {
		t.Fatalf("advisory sends = %+v, want the revision under original collapse ID %q", sent, family)
	}
}

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

func TestDispatcherRechecksDailyClosureOccurrenceOnRetry(t *testing.T) {
	for _, tc := range []struct {
		name        string
		moveArrival time.Duration
		landed      bool
		evidence    string
		wantSend    bool
	}{
		{name: "same occurrence", moveArrival: 4 * time.Minute, wantSend: true},
		{name: "daily gap", moveArrival: 20 * time.Minute},
		{name: "different occurrence", moveArrival: 24*time.Hour + 4*time.Minute},
		{name: "wheels on before gate arrival", moveArrival: 4 * time.Minute, landed: true},
		{name: "missing evidence", moveArrival: 4 * time.Minute, evidence: "missing"},
		{name: "expired evidence", moveArrival: 4 * time.Minute, evidence: "expired"},
		{name: "withdrawn evidence", moveArrival: 4 * time.Minute, evidence: "withdrawn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, owner, id := dispatcherFixture(t)
			ctx := t.Context()
			flight, err := store.Flight(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			start := now.Truncate(time.Minute).Add(-10 * time.Minute)
			end := start.Add(20 * time.Minute)
			flight.ObservedAt, flight.ScheduledArrival = now, new(now.Add(2*time.Minute))
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := store.Track(ctx, owner, id, ""); err != nil {
				t.Fatal(err)
			}
			if err := store.Register(ctx, owner, Device{Platform: "ios", Kind: "alert", Token: "fixture-device"}, now); err != nil {
				t.Fatal(err)
			}
			const stamp = "0601021504"
			advisory := feeds.Advisory{ID: "daily-closure", Source: feeds.FAASWIM, Airport: "SFO", Direction: "arrival",
				Kind: "airport_closure", ObservedAt: now,
				RawReason: fmt.Sprintf("!SFO SFO AD AP CLSD DLY %s-%s %s-%s", start.Format("1504"), end.Format("1504"),
					start.AddDate(0, 0, -1).Format(stamp), end.AddDate(0, 0, 1).Format(stamp))}
			publishDispatchAdvisory(t, store, advisory, now)
			jobs, err := store.claim(ctx, now, 1)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("initial closure jobs = %+v, error = %v", jobs, err)
			}
			window := jobs[0].message.AdvisoryWindow
			if window == nil || window.Airport != "SFO" || window.Direction != "arrival" ||
				!window.StartsAt.Equal(start) || !window.EndsAt.Equal(end) || !jobs[0].message.ExpiresAt.Equal(end) {
				t.Fatalf("closure job window = %+v, expires = %v, want %s to %s", window, jobs[0].message.ExpiresAt, start, end)
			}
			if err := store.resolve(ctx, jobs[0], &push.DeliveryError{Retryable: true}, now); err != nil {
				t.Fatal(err)
			}
			retry, err := store.claim(ctx, now.Add(time.Second), 1)
			if err != nil || len(retry) != 1 || retry[0].id != jobs[0].id || retry[0].attempts != 1 {
				t.Fatalf("retried closure jobs = %+v, error = %v", retry, err)
			}
			flight.ObservedAt, flight.EstimatedArrival = time.Now().UTC(), new(now.Add(tc.moveArrival))
			if tc.landed {
				flight.ActualLanding = new(now.Add(-time.Minute))
			}
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			switch tc.evidence {
			case "missing":
				if _, err := store.db.ExecContext(ctx, `DELETE FROM pushed_advisories WHERE source=? AND id=?`, advisory.Source, advisory.ID); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := store.db.ExecContext(ctx, `UPDATE pushed_advisories SET expires_ns=? WHERE source=? AND id=?`, now.UnixNano(), advisory.Source, advisory.ID); err != nil {
					t.Fatal(err)
				}
			case "withdrawn":
				withdrawal := advisory
				withdrawal.Kind, withdrawal.ObservedAt = "cancelled", flight.ObservedAt
				publishDispatchAdvisory(t, store, withdrawal, flight.ObservedAt)
			}
			sent := false
			sender := pushFunc(func(_ context.Context, _ push.Target, msg push.Message) error {
				if msg.Title != "SFO · airport closure" || msg.AdvisoryWindow != nil || !msg.ExpiresAt.Equal(end) {
					t.Fatalf("unexpected provider message: %+v", msg)
				}
				sent = true
				return nil
			})
			store.deliver(ctx, sender, slog.New(slog.DiscardHandler), retry[0])
			if sent != tc.wantSend {
				t.Fatalf("closure alert sent = %t, want %t", sent, tc.wantSend)
			}
			if !sent {
				var pending int
				if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE id=?`, retry[0].id).Scan(&pending); err != nil || pending != 0 {
					t.Fatalf("obsolete closure job rows = %d, error = %v", pending, err)
				}
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
