package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
	"github.com/crizzy9/plano/internal/push"
)

func TestAirportProgramContextAlertsWithoutAttributingAFlightDelay(t *testing.T) {
	store, owner, id := dispatcherFixture(t)
	ctx := t.Context()
	now := time.Now().UTC()
	flight := noticeFlight(now)
	flight.ID, flight.ObservedAt = id, now
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, id, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, owner, Device{Platform: "android", Kind: "alert", Token: "program-fixture"}, now); err != nil {
		t.Fatal(err)
	}
	program := feeds.Advisory{
		ID: "tfms/gs/EWR", Source: feeds.FAASWIM, Airport: "EWR", Kind: "ground_stop", Direction: "arrival",
		Reason:   "FAA reports a ground stop for selected flights due to thunderstorms. Check with your airline.",
		StartsAt: new(now.Add(-8 * time.Hour)), EndsAt: new(now.Add(time.Hour)),
		ObservedAt: now.Add(-8 * time.Hour), AirportContextOnly: true,
	}
	publishDispatchAdvisory(t, store, program, now)
	publishDispatchAdvisory(t, store, program, now)
	notices, err := store.FlightNotices(ctx, flight, now)
	if err != nil || len(notices) != 1 || notices[0].Operation != "arrival" || notices[0].Summary != program.Reason ||
		!notices[0].ObservedAt.Equal(program.ObservedAt) || !notices[0].ValidUntil.Equal(*program.EndsAt) {
		t.Fatalf("airport program notices = %+v, error = %v", notices, err)
	}
	taxi := flight
	taxi.Status, taxi.ActualDeparture, taxi.ScheduledArrival = "taxi_out", new(now.Add(-90*time.Minute)), nil
	if _, ok := advisoryEvent(taxi, program, now); !ok {
		t.Fatal("ground stop was suppressed during taxi-out")
	}
	if notices, err := store.FlightNotices(ctx, taxi, now); err != nil || len(notices) != 1 {
		t.Fatalf("taxi-out program notices = %+v, error = %v", notices, err)
	}
	taxi.Status = "airborne"
	if _, ok := advisoryEvent(taxi, program, now); ok {
		t.Fatal("ground stop alert stayed eligible after takeoff")
	}
	if features, ok := ExtractDelayFeatures(flight, nil, []feeds.Advisory{program}, now); !ok || features != (predict.Features{}) {
		t.Fatalf("airport context entered delay model inputs: %+v, %t", features, ok)
	}
	if prediction := Predict(flight, nil, []feeds.Advisory{program}, now); prediction != nil {
		t.Fatalf("airport context generated a flight forecast: %+v", prediction)
	}
	jobs, err := store.claim(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	sent := 0
	for _, job := range jobs {
		store.deliver(ctx, pushFunc(func(_ context.Context, _ push.Target, msg push.Message) error {
			if msg.Title == "EWR · ground stop" && msg.Body == program.Reason {
				if !msg.ExpiresAt.Equal(now.Add(advisoryForecastTTL)) {
					t.Fatalf("airport alert expiry = %s, want independent push freshness limit", msg.ExpiresAt)
				}
				sent++
			}
			return nil
		}), slog.New(slog.DiscardHandler), job)
	}
	if sent != 1 {
		t.Fatalf("airport program sent %d alerts, want 1", sent)
	}
	program.ObservedAt = time.Now().UTC()
	program.EndsAt = new(now.Add(90 * time.Minute))
	publishDispatchAdvisory(t, store, program, program.ObservedAt)
	jobs, err = store.claim(ctx, time.Now(), 10)
	if err != nil || len(jobs) != 1 || jobs[0].message.AdvisoryWindow == nil {
		t.Fatalf("revised program claims = %+v, error = %v", jobs, err)
	}
	flight.ScheduledDeparture, flight.ObservedAt = new(now.Add(2*time.Hour)), time.Now().UTC()
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	store.deliver(ctx, pushFunc(func(context.Context, push.Target, push.Message) error {
		t.Fatal("ground stop reached transport after the flight moved outside the hold window")
		return nil
	}), slog.New(slog.DiscardHandler), jobs[0])
}

func TestTFMSProgramRevisionOrderingSurvivesPrune(t *testing.T) {
	for _, revision := range []string{"withdrawal", "linked withdrawal", "polled linked withdrawal", "shortened end"} {
		t.Run(revision, func(t *testing.T) {
			ctx := t.Context()
			store, _, _ := dispatcherFixture(t)
			now := time.Now().UTC()
			program := feeds.Advisory{ID: "tfms/gdp/EWR", Source: feeds.FAASWIM, Kind: "ground_delay", Airport: "EWR",
				Direction: "arrival", AirportContextOnly: true, ObservedAt: now.Add(-10 * time.Hour),
				StartsAt: new(now.Add(-10 * time.Hour)), EndsAt: new(now.Add(3 * time.Hour))}
			changed := feeds.Advisory{ID: program.ID, Source: program.Source, Airport: program.Airport,
				Kind: "cancelled", ObservedAt: now.Add(-8 * time.Hour)}
			switch revision {
			case "linked withdrawal", "polled linked withdrawal":
				changed.ID, changed.SupersedesID = "withdrawal/fixture", program.ID
			case "shortened end":
				changed = program
				changed.ObservedAt, changed.EndsAt = now.Add(-8*time.Hour), new(now.Add(-9*time.Hour))
			}
			publishDispatchAdvisory(t, store, program, program.ObservedAt)
			if revision == "polled linked withdrawal" {
				if err := store.cachePolledFlightNotices(ctx, program.Source, []feeds.Advisory{changed}, now); err != nil {
					t.Fatal(err)
				}
				if err := store.cachePolledFlightNotices(ctx, program.Source, nil, now); err != nil {
					t.Fatal(err)
				}
			} else {
				publishDispatchAdvisory(t, store, changed, changed.ObservedAt)
			}
			if err := store.Prune(ctx, now); err != nil {
				t.Fatal(err)
			}
			publishDispatchAdvisory(t, store, program, now)
			if notices, err := store.FlightNotices(ctx, noticeFlight(now), now); err != nil || len(notices) != 0 {
				t.Fatalf("stale program resurfaced in flight details: %+v, error = %v", notices, err)
			}
			rows, err := store.pushedAdvisories(ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			merged, err := store.filterSupersededAdvisories(ctx, mergeAdvisories([]feeds.Advisory{program}, rows, now), now)
			if err != nil || len(merged) != 0 {
				t.Fatalf("stale program resurfaced in merged advisories: %+v, error = %v", merged, err)
			}
			if err := store.Prune(ctx, now.Add(tfmsAdvisoryRetention)); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := store.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM pushed_advisories)+(SELECT COUNT(*) FROM advisory_supersessions)`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("expired ordering markers = %d, error = %v", count, err)
			}
		})
	}
}

func TestTFMSLegacyCutoffDoesNotAdmitUnseenOldProgram(t *testing.T) {
	ctx := t.Context()
	store, _, _ := dispatcherFixture(t)
	now := time.Now().UTC()
	cutoff := now.Add(-signedAdvisoryRetention)
	store.tfmsLegacyReplayCutoff = cutoff.UnixNano()
	program := feeds.Advisory{ID: "tfms/gdp/EWR", Source: feeds.FAASWIM, Kind: "ground_delay", Airport: "EWR",
		Direction: "arrival", AirportContextOnly: true, ObservedAt: cutoff,
		StartsAt: &cutoff, EndsAt: new(now.Add(3 * time.Hour))}
	publishDispatchAdvisory(t, store, program, now)
	if err := store.cachePolledFlightNotices(ctx, program.Source, []feeds.Advisory{program}, now); err != nil {
		t.Fatal(err)
	}
	if notices, err := store.FlightNotices(ctx, noticeFlight(now), now); err != nil || len(notices) != 0 {
		t.Fatalf("unknown program from before cutover entered notices: %+v, error = %v", notices, err)
	}
	program.ObservedAt = cutoff.Add(time.Nanosecond)
	publishDispatchAdvisory(t, store, program, now)
	if notices, err := store.FlightNotices(ctx, noticeFlight(now), now); err != nil || len(notices) != 1 ||
		!notices[0].ObservedAt.Equal(program.ObservedAt) {
		t.Fatalf("newer program past legacy boundary did not enter notices: %+v, error = %v", notices, err)
	}
}

func TestAdvisoryEventChecksPublishedNOTAMWindow(t *testing.T) {
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	start, end := now.Add(5*time.Minute), now.Add(20*time.Minute)
	flight := feeds.Flight{ID: "flight", Status: "scheduled", Origin: "SFO", OriginICAO: "KSFO", Destination: "JFK", DestinationICAO: "KJFK",
		ScheduledDeparture: new(now.Add(10 * time.Minute))}
	const fixed = "(A1234/26 NOTAMN\nA) KSFO B) 2611051805 C) 2611051820\nE) AD CLSD)"
	const estimated = "(A1234/26 NOTAMN\nA) KSFO B) 2611051805 C) 2611051820EST\nE) AD CLSD)"
	for _, tc := range []struct {
		name, raw  string
		start, end *time.Time
		wantAlert  bool
	}{
		{"matching normalized window", fixed, &start, &end, true},
		{"fixed raw window only", fixed, nil, nil, true},
		{"published window at arrival airport", strings.Replace(fixed, "KSFO", "KJFK", 1), nil, nil, false},
		{"conflicting normalized window", fixed, &start, new(end.Add(5 * time.Minute)), false},
		{"estimated published window", estimated, &start, &end, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			advisory := feeds.Advisory{ID: "closure", Kind: "airport_closure", Source: feeds.FAASWIM, Airport: "KSFO",
				RawReason: tc.raw, ObservedAt: now, StartsAt: tc.start, EndsAt: tc.end}
			event, ok := advisoryEvent(flight, advisory, now)
			if ok != tc.wantAlert {
				t.Fatalf("alert eligibility = %t, want %t", ok, tc.wantAlert)
			}
			if !ok {
				return
			}
			if event.validUntil == nil || !event.validUntil.Equal(end) {
				t.Fatalf("alert validity = %v, want %s", event.validUntil, end)
			}
			if len(event.advisoryEvidence) != 1 || event.advisoryEvidence[0] != advisoryEvidence(advisory) {
				t.Fatalf("alert did not retain original advisory evidence: %+v", event.advisoryEvidence)
			}
		})
	}
}

func TestDailyClosureAlertsFollowThePublishedOccurrence(t *testing.T) {
	now := time.Date(2026, 11, 6, 1, 20, 0, 0, time.UTC)
	ends := now.Add(25 * time.Minute)
	flight := noticeFlight(now)
	row := feeds.Advisory{ID: "closure", Kind: "airport_closure", Source: feeds.FAASWIM,
		Airport: "KSFO", ObservedAt: now,
		RawReason: "(A1234/26 NOTAMN\nA) KSFO B) 2611052200 C) 2611070145\nD) DLY 2200-0145\nE) AD CLSD)"}
	first, ok := advisoryEvent(flight, row, now)
	if !ok || first.validUntil == nil || !first.validUntil.Equal(ends) ||
		first.advisoryWindow == nil || !first.advisoryWindow.EndsAt.Equal(ends) || first.advisoryWindow.Direction != "departure" ||
		!strings.Contains(first.Body, "Nov 6 at 01:45 UTC") {
		t.Fatalf("overnight event = %+v, %t", first, ok)
	}
	if len(first.advisoryEvidence) != 1 || first.advisoryEvidence[0] != advisoryEvidence(row) {
		t.Fatalf("daily alert rewrote source evidence: %+v", first.advisoryEvidence)
	}
	flight.EstimatedDeparture = &ends
	if _, ok := advisoryEvent(flight, row, now); ok {
		t.Fatal("closure alert included the published reopening time")
	}
	flight.EstimatedDeparture = new(ends.Add(23 * time.Hour))
	next, ok := advisoryEvent(flight, row, now)
	if !ok || next.ID == first.ID || next.collapseID != first.collapseID ||
		next.advisoryWindow == nil || !next.advisoryWindow.EndsAt.Equal(ends.Add(24*time.Hour)) {
		t.Fatalf("following occurrence event = %+v, %t", next, ok)
	}
	legacy := row
	legacy.Kind = "airport_restriction"
	if _, ok := advisoryEvent(flight, legacy, now); ok {
		t.Fatal("legacy daily restriction queued an alert before source refresh")
	}
	row.EndsAt = new(ends.Add(24*time.Hour + time.Minute))
	if _, ok := advisoryEvent(flight, row, now); ok {
		t.Fatal("conflicting normalized end queued a daily closure alert")
	}
}

func TestEndedAdvisorySurvivesDelayedReplayAndSuppressesOldPoll(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "advisory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	oldEnd := now.Add(time.Hour)
	old := feeds.Advisory{ID: "SWIM-1", Source: feeds.FAASWIM, Airport: "JFK", Kind: "ground_stop", ObservedAt: now.Add(-3 * time.Hour), EndsAt: &oldEnd}
	ended := old
	end := now.Add(-2 * time.Hour)
	ended.ObservedAt, ended.EndsAt = end, &end
	if err := s.PublishAdvisory(ctx, ended, end); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishAdvisory(ctx, old, now); err != nil {
		t.Fatal(err)
	}
	revisions, err := s.pushedAdvisories(ctx, now)
	if err != nil || len(revisions) != 1 || !revisions[0].advisory.ObservedAt.Equal(end) {
		t.Fatalf("ended revision lost: %v %v", revisions, err)
	}
	if result := mergeAdvisories([]feeds.Advisory{old}, revisions, now); len(result) != 0 {
		t.Fatalf("stale advisory revived: %v", result)
	}
}

func TestFutureAdvisoryRevisionDoesNotReplaceCurrentEvidence(t *testing.T) {
	now := time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC)
	current := feeds.Advisory{ID: "revision", Source: feeds.FAASWIM, ObservedAt: now.Add(-time.Minute)}
	future := current
	future.ObservedAt = now.Add(15 * time.Second)
	end := now.Add(time.Hour).UnixNano()
	for _, tc := range []struct {
		name   string
		polled feeds.Advisory
		pushed feeds.Advisory
	}{
		{"future poll", future, current},
		{"future callback", current, future},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeAdvisories([]feeds.Advisory{tc.polled}, []advisoryRevision{{advisory: tc.pushed, expiresNS: end}}, now)
			if len(got) != 1 || !got[0].ObservedAt.Equal(current.ObservedAt) {
				t.Fatalf("future revision displaced current evidence: %+v", got)
			}
		})
	}
}

func TestMaterialAdvisoryRevisionSupersedesPendingAlert(t *testing.T) {
	ctx := t.Context()
	store, owner, id := dispatcherFixture(t)
	now := time.Now().UTC()
	flight, err := store.Flight(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	flight.ObservedAt, flight.ScheduledDeparture = now.Add(time.Millisecond), new(now.Add(25*time.Hour))
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, id, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, owner, Device{Platform: "ios", Kind: "alert", Token: "revision-test"}, now); err != nil {
		t.Fatal(err)
	}
	initial := feeds.Advisory{ID: "airport-program", Source: feeds.FAASWIM, Airport: "JFK", Kind: "departure_delay",
		Direction: "departure", Reason: "low visibility", ObservedAt: now.Add(-time.Minute)}
	repeated := initial
	repeated.ObservedAt = now.Add(-30 * time.Second)
	revised := repeated
	revised.ObservedAt, revised.Reason = now.Add(-time.Second), "runway congestion"
	future := revised
	future.ObservedAt, future.Reason = now.Add(15*time.Second), "future program state"
	for _, advisory := range []feeds.Advisory{initial, repeated, future, revised} {
		if err := store.PublishAdvisory(ctx, advisory, now); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := store.claim(ctx, now, 4)
	if err != nil || len(jobs) != 2 || jobs[0].message.CollapseID != jobs[1].message.CollapseID {
		t.Fatalf("unexpected revision jobs: %+v %v", jobs, err)
	}
	var delivered []string
	sender := pushFunc(func(_ context.Context, _ push.Target, message push.Message) error {
		delivered = append(delivered, message.Body)
		return nil
	})
	for _, job := range jobs {
		store.deliver(ctx, sender, slog.New(slog.DiscardHandler), job)
	}
	if len(delivered) != 1 || delivered[0] != revised.Reason {
		t.Fatalf("wrong program revision delivered: %v", delivered)
	}
}

func TestIntermediateFutureAdvisoryWithdrawalBecomesEffective(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "source order"
		if reverse {
			name = "reverse arrival order"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			store, err := Open(filepath.Join(t.TempDir(), "advisory.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			now := time.Now().UTC()
			initial := dispatchAdvisoryFixture(now)
			withdrawal := initial
			withdrawal.ObservedAt, withdrawal.Kind = now.Add(10*time.Second), "cancelled"
			reinstated := initial
			reinstated.ObservedAt = now.Add(20 * time.Second)
			rows := []feeds.Advisory{initial, withdrawal, reinstated}
			if reverse {
				rows[1], rows[2] = rows[2], rows[1]
			}
			for _, row := range rows {
				if err := store.PublishAdvisory(ctx, row, now); err != nil {
					t.Fatal(err)
				}
			}
			forecast := &Prediction{AdvisoryEvidence: []forecastAdvisory{advisoryEvidence(initial)}}
			for _, stage := range []struct {
				at    time.Time
				want  time.Time
				stale bool
			}{
				{now, initial.ObservedAt, false},
				{withdrawal.ObservedAt, withdrawal.ObservedAt, true},
				{reinstated.ObservedAt, reinstated.ObservedAt, false},
			} {
				current, err := store.pushedAdvisories(ctx, stage.at)
				if err != nil || len(current) != 1 || !current[0].advisory.ObservedAt.Equal(stage.want) {
					t.Fatalf("wrong effective revision at %s: %+v %v", stage.at, current, err)
				}
				if stale, err := forecastAdvisoryRevised(ctx, store.db, forecast, stage.at); err != nil || stale != stage.stale {
					t.Fatalf("wrong warning validity at %s: %v %v", stage.at, stale, err)
				}
			}
		})
	}
}

func TestFutureSignedRevisionPreservesEffectiveWithdrawal(t *testing.T) {
	for _, futureFirst := range []bool{false, true} {
		name := "withdrawal first"
		if futureFirst {
			name = "future revision first"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			store, err := Open(filepath.Join(t.TempDir(), "advisory.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			now := time.Now().UTC()
			initial := dispatchAdvisoryFixture(now)
			withdrawal := initial
			withdrawal.Kind, withdrawal.ObservedAt = "cancelled", now.Add(-time.Second)
			future := initial
			future.ObservedAt = now.Add(15 * time.Second)
			observations := []feeds.Advisory{initial, withdrawal, future}
			if futureFirst {
				observations[1], observations[2] = observations[2], observations[1]
			}
			for _, observation := range observations {
				if err := store.PublishAdvisory(ctx, observation, now); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.pushedAdvisories(ctx, now)
			if err != nil || len(before) != 1 || before[0].advisory.Kind != "cancelled" {
				t.Fatalf("effective withdrawal was lost: %+v %v", before, err)
			}
			forecast := &Prediction{AdvisoryEvidence: []forecastAdvisory{advisoryEvidence(initial)}}
			if stale, err := forecastAdvisoryRevised(ctx, store.db, forecast, now); err != nil || !stale {
				t.Fatalf("older warning evidence was revived: %v %v", stale, err)
			}
			after, err := store.pushedAdvisories(ctx, future.ObservedAt)
			if err != nil || len(after) != 1 || !after[0].advisory.ObservedAt.Equal(future.ObservedAt) {
				t.Fatalf("new revision failed to become current: %+v %v", after, err)
			}
			if err := store.Prune(ctx, future.ObservedAt); err != nil {
				t.Fatal(err)
			}
			var predecessors int
			if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM pushed_advisory_predecessors`).Scan(&predecessors); err != nil || predecessors != 0 {
				t.Fatalf("predecessor survived activation: %d %v", predecessors, err)
			}
		})
	}
}
