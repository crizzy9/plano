package core

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestPassportArchiveIsolationPagingAndTrackedOverlap(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner, other := friendInstallation(t, s), friendInstallation(t, s)
	departure := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	arrival := departure.Add(3 * time.Hour)
	observed := arrival.Add(time.Hour)
	tracked := feeds.Flight{
		ID: "UAL123-completed", Source: "flightaware_aeroapi", Number: "UA123", Status: "arrived",
		Origin: "JFK", OriginICAO: "KJFK", Destination: "ORD", DestinationICAO: "KORD",
		ObservedAt: observed, ScheduledDeparture: &departure, ActualArrival: &arrival,
	}
	if err := s.Observe(ctx, tracked, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, tracked.ID, ""); err != nil {
		t.Fatal(err)
	}

	local := departure.In(time.FixedZone("-0400", -4*60*60))
	overlap := feeds.Flight{
		ID: "archive-" + strings.Repeat("a", 64), Source: "flighty_csv", Number: tracked.Number, Status: "arrived",
		Origin: "KJFK", Destination: "ORD", ObservedAt: observed, ScheduledDeparture: &local,
		HistoryDate: "2026-06-10",
	}
	dateOnly := overlap
	dateOnly.ID, dateOnly.HistoryDate, dateOnly.ScheduledDeparture = "archive-"+strings.Repeat("b", 64), "2026-06-11", nil
	flights := []feeds.Flight{overlap, dateOnly}
	if added, duplicates, err := s.ImportPassportArchive(ctx, owner, flights); err != nil || added != 2 || duplicates != 0 {
		t.Fatalf("first import added %d, duplicates %d, error %v", added, duplicates, err)
	}
	if added, duplicates, err := s.ImportPassportArchive(ctx, owner, flights); err != nil || added != 0 || duplicates != 2 {
		t.Fatalf("retry added %d, duplicates %d, error %v", added, duplicates, err)
	}
	if added, _, err := s.ImportPassportArchive(ctx, other, []feeds.Flight{overlap}); err != nil || added != 1 {
		t.Fatalf("other installation added %d, error %v", added, err)
	}
	if history, err := s.CompletedFlights(ctx, other); err != nil || len(history) != 1 || history[0].ID != overlap.ID {
		t.Fatalf("other history = %+v, error %v", history, err)
	}
	if history, err := s.CompletedFlights(ctx, owner); err != nil || len(history) != 2 || history[0].ID != dateOnly.ID || history[1].ID != tracked.ID {
		t.Fatalf("owner history = %+v, error %v", history, err)
	}
	first, cursor, err := s.CompletedFlightPage(ctx, owner, nil, 1)
	if err != nil || len(first) != 1 || cursor == nil || cursor.ID != "a."+dateOnly.ID || first[0].Flight.ScheduledDeparture != nil || first[0].Flight.ActualArrival != nil {
		t.Fatalf("first page = %+v, cursor %+v, error %v", first, cursor, err)
	}
	if second, cursor, err := s.CompletedFlightPage(ctx, owner, cursor, 1); err != nil || len(second) != 1 || cursor != nil || second[0].Flight.ID != tracked.ID {
		t.Fatalf("second page = %+v, cursor %+v, error %v", second, cursor, err)
	}
	if _, err := s.Flight(ctx, dateOnly.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archive reached the shared flight cache: %v", err)
	}
	if err := s.DeletePassportArchive(ctx, other, dateOnly.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner delete: %v", err)
	}
	if err := s.DeletePassportArchive(ctx, owner, dateOnly.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePassportArchive(ctx, owner, dateOnly.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeat delete: %v", err)
	}
	if err := s.Untrack(ctx, owner, tracked.ID); err != nil {
		t.Fatal(err)
	}
	if history, err := s.CompletedFlights(ctx, owner); err != nil || len(history) != 1 || history[0].ID != overlap.ID {
		t.Fatalf("owner archive after untracking = %+v, error %v", history, err)
	}
}

func TestPassportArchiveCapRollsBackTheEntireImport(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "archive-cap.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner := friendInstallation(t, s)
	if _, err := s.db.ExecContext(ctx, `WITH RECURSIVE n(value) AS (
  SELECT 1 UNION ALL SELECT value+1 FROM n WHERE value<?
) INSERT INTO passport_archive(owner,id,flight_date,snapshot)
SELECT ?, 'archive-'||printf('%064x',value), '2026-01-01', '{}' FROM n`, maxPassportArchiveFlights-1, owner); err != nil {
		t.Fatal(err)
	}
	flight := feeds.Flight{ID: "archive-" + strings.Repeat("a", 64), Source: "flighty_csv", Status: "arrived", HistoryDate: "2026-06-11"}
	other := flight
	other.ID = "archive-" + strings.Repeat("b", 64)
	if _, _, err := s.ImportPassportArchive(ctx, owner, []feeds.Flight{flight, other}); !errors.Is(err, ErrPassportArchiveFull) {
		t.Fatalf("over-cap import error = %v", err)
	}
	if added, duplicates, err := s.ImportPassportArchive(ctx, owner, []feeds.Flight{flight}); err != nil || added != 1 || duplicates != 0 {
		t.Fatalf("rolled-back row added %d, duplicates %d, error %v", added, duplicates, err)
	}
	if added, duplicates, err := s.ImportPassportArchive(ctx, owner, []feeds.Flight{flight}); err != nil || added != 0 || duplicates != 1 {
		t.Fatalf("retry at capacity added %d, duplicates %d, error %v", added, duplicates, err)
	}
}
