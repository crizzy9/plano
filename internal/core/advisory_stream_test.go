package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
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
