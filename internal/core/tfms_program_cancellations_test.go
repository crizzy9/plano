package core

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestTFMSProgramCancellationStorageAndScope(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "tfms-cancellations.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	start, end := now.Add(10*time.Minute), now.Add(20*time.Minute)
	withdrawal := func(airport, serial string, observed, ends time.Time) feeds.Advisory {
		return feeds.Advisory{
			Source: feeds.FAASWIM, Kind: feeds.AdvisoryTFMSCancellation,
			ID: "tfms/cnx/gs/" + airport + "/20261106/" + serial, TFMSProgramID: "tfms/gs/" + airport,
			Airport: airport, Direction: "arrival", AirportContextOnly: true,
			ObservedAt: observed, StartsAt: &start, EndsAt: &ends,
		}
	}
	record := func(a feeds.Advisory) error {
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := recordTFMSProgramCancellation(ctx, tx, a); err != nil {
			return err
		}
		return tx.Commit()
	}
	original := withdrawal("LGA", "001", now.Add(-time.Second), end)
	for _, a := range []feeds.Advisory{
		original,
		original,
		withdrawal("KLGA", "002", now, end),
		withdrawal("LGA", "003", now.Add(time.Second), end),
		withdrawal("EWR", "004", now, end),
	} {
		if err := record(a); err != nil {
			t.Fatal(err)
		}
	}
	expired := withdrawal("LGA", "005", now.Add(-time.Hour), now)
	expired.StartsAt = &expired.ObservedAt
	if err := record(expired); err != nil {
		t.Fatal(err)
	}
	conflict := original
	conflictingEnd := end.Add(time.Hour)
	conflict.EndsAt = &conflictingEnd
	if err := record(conflict); !errors.Is(err, feeds.ErrInvalidQuery) {
		t.Fatalf("conflicting replay error = %v, want invalid query", err)
	}
	rows, err := loadTFMSProgramCancellations(ctx, store.db, "LGA", "KLGA", now)
	if err != nil || len(rows) != 2 {
		t.Fatalf("effective destination cancellations = %v, error = %v", rows, err)
	}
	older := now.Add(-time.Minute)
	for _, tc := range []struct {
		name, program       string
		revision, operation time.Time
		want                bool
	}{
		{"first instant", original.TFMSProgramID, older, start, true},
		{"preceding instant", original.TFMSProgramID, older, start.Add(-time.Nanosecond), false},
		{"last instant", original.TFMSProgramID, older, end.Add(-time.Nanosecond), true},
		{"exclusive end after rejected replay", original.TFMSProgramID, older, end, false},
		{"same revision time", original.TFMSProgramID, original.ObservedAt, start, false},
		{"newer revision", original.TFMSProgramID, now, start, false},
		{"same airport different program", "tfms/gdp/LGA", older, start, false},
		{"different airport", "tfms/gs/EWR", older, start, false},
		{"ICAO airport scope", "tfms/gs/KLGA", older, start, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := latestTFMSProgramCancellation(rows, tc.program, tc.operation).After(tc.revision); got != tc.want {
				t.Errorf("operation cancelled = %t, want %t", got, tc.want)
			}
		})
	}
	rows, err = loadTFMSProgramCancellations(ctx, store.db, "LGA", "", now.Add(time.Second))
	if err != nil || len(rows) != 2 || !latestTFMSProgramCancellation(rows, original.TFMSProgramID, start).After(now) {
		t.Fatalf("future cancellation did not become effective: %v, error = %v", rows, err)
	}
}
