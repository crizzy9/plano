package core

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestManualPassportFlightCreateAndUpdateStayScoped(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "manual-archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner, other := friendInstallation(t, s), friendInstallation(t, s)
	manual := feeds.Flight{
		ID: "archive-" + strings.Repeat("c", 64), Source: feeds.PassportManualSource, Status: "arrived",
		Origin: "JFK", Destination: "ORD", HistoryDate: "2000-01-02",
		ObservedAt: time.Date(2026, 6, 10, 14, 0, 0, 123456789, time.UTC),
	}
	if created, err := s.CreateManualPassportFlight(ctx, owner, manual); err != nil || !created {
		t.Fatalf("manual create = %v, %v", created, err)
	}
	if _, err := s.Flight(ctx, manual.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("manual entry reached the shared flight cache: %v", err)
	}
	assertHistory := func(installation string, want ...feeds.Flight) {
		t.Helper()
		history, err := s.CompletedFlights(ctx, installation)
		if err != nil || len(history) != len(want) {
			t.Fatalf("history = %+v, want %+v, error %v", history, want, err)
		}
		for i, flight := range history {
			if !reflect.DeepEqual(flight, want[i]) {
				t.Fatalf("history entry = %+v, want %+v", flight, want[i])
			}
		}
	}
	assertHistory(other)
	csvCollision := manual
	csvCollision.Source = "flighty_csv"
	csvCollision.ActualDeparture = new(manual.ObservedAt.Add(-time.Hour))
	if added, duplicates, err := s.ImportPassportArchive(ctx, owner, []feeds.Flight{csvCollision}); err != nil || added != 0 || duplicates != 1 {
		t.Fatalf("CSV collision added %d, duplicates %d, error %v", added, duplicates, err)
	}
	assertHistory(owner, manual)
	updated := manual
	updated.Destination, updated.HistoryDate = "SFO", "2000-01-03"
	if err := s.UpdateManualPassportFlight(ctx, other, updated, manual.ObservedAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner update error = %v", err)
	}
	if err := s.UpdateManualPassportFlight(ctx, owner, updated, manual.ObservedAt.Add(-time.Nanosecond)); !errors.Is(err, ErrPassportArchiveConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	assertHistory(owner, manual)
	// Even if the clock is behind, editing creates a new version.
	updated.ObservedAt = manual.ObservedAt.Add(-time.Second)
	if err := s.UpdateManualPassportFlight(ctx, owner, updated, manual.ObservedAt); err != nil {
		t.Fatal(err)
	}
	want := updated
	want.ObservedAt = manual.ObservedAt.Add(time.Nanosecond)
	assertHistory(owner, want)
	if err := s.UpdateManualPassportFlight(ctx, owner, updated, manual.ObservedAt); err != nil {
		t.Fatalf("identical edit retry = %v", err)
	}
	if created, err := s.CreateManualPassportFlight(ctx, owner, manual); err != nil || created {
		t.Fatalf("create replay = %v, %v", created, err)
	}
	assertHistory(owner, want)
	if created, err := s.CreateManualPassportFlight(ctx, other, manual); err != nil || !created {
		t.Fatalf("independent installation create = %v, %v", created, err)
	}
	assertHistory(other, manual)
	if err := s.DeletePassportArchive(ctx, owner, manual.ID); err != nil {
		t.Fatal(err)
	}
	assertHistory(owner)
	assertHistory(other, manual)
}

func TestManualPassportFlightCannotEditCSVArchive(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "manual-csv-archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner := friendInstallation(t, s)
	csv := feeds.Flight{
		ID: "archive-" + strings.Repeat("d", 64), Source: "flighty_csv", Status: "arrived",
		Origin: "JFK", Destination: "ORD", HistoryDate: "2000-01-02",
		ObservedAt: time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC),
	}
	if _, _, err := s.ImportPassportArchive(ctx, owner, []feeds.Flight{csv}); err != nil {
		t.Fatal(err)
	}
	edit := csv
	edit.Source, edit.Destination = feeds.PassportManualSource, "SFO"
	if err := s.UpdateManualPassportFlight(ctx, owner, edit, csv.ObservedAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CSV edit error = %v", err)
	}
	if history, err := s.CompletedFlights(ctx, owner); err != nil || len(history) != 1 || !reflect.DeepEqual(history[0], csv) {
		t.Fatalf("CSV archive changed: %+v, %v", history, err)
	}
}

func TestPassportArchiveReimportFillsMissingTimes(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "archive-reimport.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner, other := friendInstallation(t, s), friendInstallation(t, s)
	legacy := feeds.Flight{
		ID: "archive-" + strings.Repeat("a", 64), Source: "flighty_csv", Number: "UAL14", Status: "arrived",
		Origin: "EWR", Destination: "LHR", HistoryDate: "2026-01-02", AircraftModel: "Boeing 787-9",
		ObservedAt: time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC),
	}
	for _, installation := range []string{owner, other} {
		if added, duplicates, err := s.ImportPassportArchive(ctx, installation, []feeds.Flight{legacy}); err != nil || added != 1 || duplicates != 0 {
			t.Fatalf("seed added %d, duplicates %d, error %v", added, duplicates, err)
		}
	}
	assertHistory := func(installation string, want feeds.Flight) {
		t.Helper()
		history, err := s.CompletedFlights(ctx, installation)
		if err != nil || len(history) != 1 || !reflect.DeepEqual(history[0], want) {
			t.Fatalf("history = %+v, want %+v, error %v", history, want, err)
		}
	}
	reimport := func(flight feeds.Flight) {
		t.Helper()
		if added, duplicates, err := s.ImportPassportArchive(ctx, owner, []feeds.Flight{flight}); err != nil || added != 0 || duplicates != 1 {
			t.Fatalf("reimport added %d, duplicates %d, error %v", added, duplicates, err)
		}
	}
	departure := time.Date(2026, 1, 2, 13, 50, 0, 0, time.UTC)
	arrival := time.Date(2026, 1, 2, 21, 5, 0, 0, time.UTC)
	want := legacy
	want.ScheduledDeparture = &departure
	want.ScheduledArrival = &arrival
	want.ActualDeparture = new(departure.Add(5 * time.Minute))
	want.ActualArrival = new(arrival.Add(5 * time.Minute))
	updated := want
	updated.ObservedAt = legacy.ObservedAt.Add(time.Hour)
	updated.AircraftModel = "Boeing 777-200"
	for _, conflict := range []func(*feeds.Flight){
		func(f *feeds.Flight) { f.HistoryDate = "2026-01-03" },
		func(f *feeds.Flight) { f.Number = "UAL15" },
		func(f *feeds.Flight) { f.Origin = "JFK" },
		func(f *feeds.Flight) { f.Destination = "LGW" },
	} {
		copy := updated
		conflict(&copy)
		reimport(copy)
		assertHistory(owner, legacy)
	}
	partial := updated
	partial.ScheduledArrival, partial.ActualArrival = nil, nil
	reimport(partial)
	partialWant := want
	partialWant.ScheduledArrival, partialWant.ActualArrival = nil, nil
	assertHistory(owner, partialWant)

	conflicting := updated
	conflicting.ScheduledDeparture = new(departure.Add(time.Minute))
	conflicting.ActualDeparture = nil
	conflicting.ActualArrival = new(departure.Add(time.Minute))
	reimport(conflicting)
	assertHistory(owner, partialWant)

	updated.ScheduledDeparture = new(departure.In(time.FixedZone("export", 3600)))
	reimport(updated)
	assertHistory(owner, want)
	assertHistory(other, legacy)

	updated.ActualArrival = new(arrival.Add(15 * time.Minute))
	reimport(updated)
	assertHistory(owner, want)
}

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
	gateOut, scheduledArrival := departure.Add(5*time.Minute), arrival.Add(-10*time.Minute)
	observed := arrival.Add(time.Hour)
	tracked := feeds.Flight{
		ID: "UAL123-completed", Source: "flightaware_aeroapi", Number: "UA123", ICAONumber: "UAL123", Status: "arrived",
		Origin: "JFK", OriginICAO: "KJFK", Destination: "ORD", DestinationICAO: "KORD",
		ObservedAt: observed, ScheduledDeparture: &departure,
		ActualTakeoff: new(gateOut.Add(time.Minute)), ActualLanding: new(arrival.Add(-time.Minute)),
	}
	if err := s.Observe(ctx, tracked, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, tracked.ID, ""); err != nil {
		t.Fatal(err)
	}

	local := departure.In(time.FixedZone("-0400", -4*60*60))
	overlap := feeds.Flight{
		ID: "archive-" + strings.Repeat("a", 64), Source: "flighty_csv", Number: tracked.ICAONumber, Status: "arrived",
		Origin: "KJFK", Destination: "ORD", ObservedAt: observed, ScheduledDeparture: &local,
		HistoryDate: "2026-06-10", ActualDeparture: &gateOut, ActualArrival: &arrival, ScheduledArrival: &scheduledArrival, TailNumber: "N123AA",
	}
	dateOnly := overlap
	dateOnly.ID, dateOnly.HistoryDate, dateOnly.ScheduledDeparture = "archive-"+strings.Repeat("b", 64), "2026-06-11", nil
	dateOnly.ActualDeparture, dateOnly.ActualArrival, dateOnly.ScheduledArrival = nil, nil, nil
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
	second, cursor, err := s.CompletedFlightPage(ctx, owner, cursor, 1)
	if err != nil || len(second) != 1 || cursor != nil || second[0].Flight.ID != tracked.ID {
		t.Fatalf("second page = %+v, cursor %+v, error %v", second, cursor, err)
	}
	got := second[0].Flight
	if got.Source != tracked.Source || got.HistoryDate != overlap.HistoryDate || got.TailNumber != overlap.TailNumber ||
		got.ActualDeparture == nil || !got.ActualDeparture.Equal(gateOut) || got.ActualArrival == nil || !got.ActualArrival.Equal(arrival) ||
		got.ScheduledArrival == nil || !got.ScheduledArrival.Equal(scheduledArrival) {
		t.Fatalf("tracked Passport row lost private archive measurements: %+v", got)
	}
	var arrivalCursor float64
	if err := s.db.QueryRowContext(ctx, "SELECT julianday(?)", arrival.Format(time.RFC3339)).Scan(&arrivalCursor); err != nil ||
		second[0].Cursor.ID != "p."+tracked.ID || second[0].Cursor.Arrival != arrivalCursor {
		t.Fatalf("recovered gate-in and cursor disagree: %+v, %v", second[0].Cursor, err)
	}
	if shared, err := s.Flight(ctx, tracked.ID); err != nil || shared.ActualArrival != nil || shared.TailNumber != "" || shared.HistoryDate != "" {
		t.Fatalf("private archive reached the shared flight cache: %+v, %v", shared, err)
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

func TestPassportArchiveMeasurementsRequireCompatibleUnambiguousMatch(t *testing.T) {
	for _, name := range []string{
		"compatible provider alias wins", "incompatible provider alias ignored",
		"conflicting gate-in", "archive gate-out after wheels-off",
		"multiple archives", "multiple tracked instances",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			owner := friendInstallation(t, s)
			departure := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
			gateOut, takeoff, arrival := departure.Add(5*time.Minute), departure.Add(10*time.Minute), departure.Add(3*time.Hour)
			tracked := feeds.Flight{
				ID: "UAL123-completed", Source: "flightaware_aeroapi", Number: "UA123", Status: "arrived",
				Origin: "JFK", Destination: "ORD", ObservedAt: arrival.Add(time.Hour), ScheduledDeparture: &departure, ActualTakeoff: &takeoff,
			}
			archive := tracked
			archive.ID, archive.Source, archive.HistoryDate = "archive-"+strings.Repeat("a", 64), "flighty_csv", "2026-06-10"
			archive.ActualDeparture, archive.ActualArrival, archive.TailNumber = &gateOut, &arrival, "N123AA"
			observe := func(flight feeds.Flight) {
				t.Helper()
				if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
					t.Fatal(err)
				}
			}
			wantGateOut, wantGateIn := (*time.Time)(nil), (*time.Time)(nil)
			switch name {
			case "compatible provider alias wins", "incompatible provider alias ignored":
				older := tracked
				older.ID, older.Source, older.ObservedAt = "older-alias", "flightaware_schedule", tracked.ObservedAt.Add(-time.Minute)
				if name == "compatible provider alias wins" {
					older.ActualDeparture = new(gateOut.Add(-time.Minute))
					wantGateOut = older.ActualDeparture
				} else {
					older.ActualDeparture = new(takeoff.Add(time.Minute))
					wantGateOut, wantGateIn = &gateOut, &arrival
				}
				observe(older)
				if _, err := s.db.ExecContext(ctx, "INSERT INTO aero_instance_keys VALUES(?,?)", tracked.ID, older.ID); err != nil {
					t.Fatal(err)
				}
			case "conflicting gate-in":
				tracked.ActualArrival = new(arrival.Add(time.Minute))
				wantGateIn = tracked.ActualArrival
			case "archive gate-out after wheels-off":
				archive.ActualDeparture = new(takeoff.Add(time.Minute))
			}
			observe(tracked)
			archives := []feeds.Flight{archive}
			if name == "multiple archives" {
				another := archive
				another.ID = "archive-" + strings.Repeat("b", 64)
				archives = append(archives, another)
			}
			wantCount := 1
			if name == "multiple tracked instances" {
				another := tracked
				another.ID = "UAL123-other"
				observe(another)
				wantCount = 2
			}
			if _, _, err := s.ImportPassportArchive(ctx, owner, archives); err != nil {
				t.Fatal(err)
			}
			history, err := s.CompletedFlights(ctx, owner)
			if err != nil || len(history) != wantCount {
				t.Fatalf("overlap suppression changed: %+v, %v", history, err)
			}
			for _, got := range history {
				if !sameOptionalInstant(got.ActualDeparture, wantGateOut) || !sameOptionalInstant(got.ActualArrival, wantGateIn) {
					t.Fatalf("unexpected recovered gate times: %+v", got)
				}
				if (name == "multiple archives" || name == "multiple tracked instances") && (got.HistoryDate != "" || got.TailNumber != "") {
					t.Fatalf("ambiguous archive supplied private measurements: %+v", got)
				}
			}
		})
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
	manual := other
	manual.Source, manual.ObservedAt = feeds.PassportManualSource, time.Date(2026, 6, 12, 14, 0, 0, 0, time.UTC)
	if created, err := s.CreateManualPassportFlight(ctx, owner, manual); created || !errors.Is(err, ErrPassportArchiveFull) {
		t.Fatalf("manual create at capacity = %v, %v", created, err)
	}
	if err := s.DeletePassportArchive(ctx, owner, flight.ID); err != nil {
		t.Fatal(err)
	}
	if created, err := s.CreateManualPassportFlight(ctx, owner, manual); err != nil || !created {
		t.Fatalf("manual create with a free slot = %v, %v", created, err)
	}
	if created, err := s.CreateManualPassportFlight(ctx, owner, manual); err != nil || created {
		t.Fatalf("manual retry at capacity = %v, %v", created, err)
	}
	if _, _, err := s.ImportPassportArchive(ctx, owner, []feeds.Flight{flight}); !errors.Is(err, ErrPassportArchiveFull) {
		t.Fatalf("CSV create at capacity with a manual entry = %v", err)
	}
}
