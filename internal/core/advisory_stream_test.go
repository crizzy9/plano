package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

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
