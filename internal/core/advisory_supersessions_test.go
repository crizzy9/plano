package core

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestAdvisorySupersessionSurvivesPollRemovalAndRestart(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "notices.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	old := feeds.Advisory{ID: "A0123/26", Airport: "KSFO", Source: feeds.FAASWIM, Kind: "airport_closure",
		Reason: "Earlier restriction", RawReason: "AD CLSD", ObservedAt: now.Add(-time.Minute), EndsAt: new(now.Add(time.Hour))}
	replacement := old
	replacement.ID, replacement.SupersedesID = "A0124/26", old.ID
	replacement.Reason, replacement.ObservedAt = "Replacement restriction", now.Add(10*time.Second)
	flight := noticeFlight(now)
	assertCurrent := func(at time.Time, want []string) {
		t.Helper()
		notices, err := s.FlightNotices(ctx, flight, at)
		if err != nil {
			t.Fatal(err)
		}
		var summaries []string
		for _, notice := range notices {
			summaries = append(summaries, notice.Summary)
		}
		if !slices.Equal(summaries, want) {
			t.Fatalf("notices at %s = %v, want %v", at, summaries, want)
		}
	}
	if err := s.cachePolledFlightNotices(ctx, old.Source, []feeds.Advisory{old, replacement}, now); err != nil {
		t.Fatal(err)
	}
	assertCurrent(now, []string{old.Reason})
	assertCurrent(replacement.ObservedAt, []string{replacement.Reason})
	if rows, err := s.filterSupersededAdvisories(ctx, []feeds.Advisory{old, replacement}, replacement.ObservedAt); err != nil || len(rows) != 1 || rows[0].ID != replacement.ID {
		t.Fatalf("forecast inputs = %+v, %v, want replacement", rows, err)
	}
	withdrawal := feeds.Advisory{ID: "A0125/26", Airport: old.Airport, Source: old.Source, Kind: "notam_cancellation",
		SupersedesID: replacement.ID, ObservedAt: now.Add(20 * time.Second), EndsAt: new(now.Add(20 * time.Second))}
	if err := s.PublishAdvisory(ctx, withdrawal, now.Add(25*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertCurrent(now.Add(25*time.Second), nil)
	if err := s.cachePolledFlightNotices(ctx, old.Source, nil, now.Add(25*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	replayAt := now.Add(35 * time.Second)
	if err := s.cachePolledFlightNotices(ctx, old.Source, []feeds.Advisory{old, replacement}, replayAt); err != nil {
		t.Fatal(err)
	}
	assertCurrent(replayAt, nil)
	if rows, err := s.filterSupersededAdvisories(ctx, []feeds.Advisory{old, replacement}, replayAt); err != nil || len(rows) != 0 {
		t.Fatalf("replayed forecast inputs = %+v, %v", rows, err)
	}
	if err := s.Prune(ctx, now.Add(8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM advisory_supersessions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired retirement rows = %d, %v", count, err)
	}
}

func TestAdvisorySupersessionRejectsCrossChannelRetarget(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "notices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	row := feeds.Advisory{ID: "replacement", Airport: "KSFO", Source: feeds.FAASWIM, Kind: "notam", ObservedAt: now,
		SupersedesID: "first"}
	if err := s.cachePolledFlightNotices(ctx, row.Source, []feeds.Advisory{row}, now); err != nil {
		t.Fatal(err)
	}
	conflict := row
	conflict.SupersedesID = "other"
	if err := s.PublishAdvisory(ctx, conflict, now); !errors.Is(err, feeds.ErrInvalidQuery) {
		t.Fatalf("retargeting signed successor = %v", err)
	}
	if err := s.PublishAdvisory(ctx, row, now); err != nil {
		t.Fatal(err)
	}
	conflict.ObservedAt = now.Add(time.Second)
	if err := s.cachePolledFlightNotices(ctx, row.Source, []feeds.Advisory{conflict}, now); !errors.Is(err, feeds.ErrInvalidQuery) {
		t.Fatalf("retargeting polled successor = %v", err)
	}
	var predecessor string
	if err := s.db.QueryRowContext(ctx, `SELECT predecessor_id FROM advisory_supersessions WHERE successor_id=?`, row.ID).Scan(&predecessor); err != nil || predecessor != row.SupersedesID {
		t.Fatalf("persisted predecessor = %q, %v", predecessor, err)
	}
}
