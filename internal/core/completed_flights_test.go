package core

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestCompletedFlightsResolveVerifiedAliasesBeforeFilteringStatus(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "passport.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner, other := friendInstallation(t, s), friendInstallation(t, s)
	now := time.Now().UTC()
	arrival := now.Add(-time.Hour)
	observe := func(id, source, status string, at time.Time) feeds.Flight {
		t.Helper()
		flight := feeds.Flight{ID: id, Source: source, Number: "UA1", Status: status, ObservedAt: at, ActualArrival: &arrival}
		if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		return flight
	}
	track := func(account string, flights ...feeds.Flight) {
		t.Helper()
		for _, flight := range flights {
			if err := s.Track(ctx, account, flight.ID, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	alias := func(providerID, flightID string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO aero_instance_keys VALUES(?,?)`, providerID, flightID); err != nil {
			t.Fatal(err)
		}
	}
	forget := func(flightID string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `DELETE FROM aero_instance_keys WHERE flight_id=?`, flightID); err != nil {
			t.Fatal(err)
		}
	}
	canonical := observe("UAL1-canonical", "flightaware_aeroapi", "arrived", now)
	placeholder := observe("A-schedule-placeholder", "flightaware_schedule", "arrived", now.Add(time.Second))
	alias(canonical.ID, placeholder.ID)
	forget(canonical.ID) // Legacy rows can lack their own alias record.
	downgrade := observe("schedule-downgrade", "flightaware_schedule", "scheduled", now.Add(3*time.Second))
	alias(canonical.ID, downgrade.ID)
	stale := observe("schedule-stale", "flightaware_schedule", "airborne", now.Add(2*time.Second))
	alias(canonical.ID, stale.ID)
	distinct := observe("UAL1-next-instance", "flightaware_aeroapi", "arrived", now)
	corrected := observe("UAL1-correction", "flightaware_aeroapi", "cancelled", now.Add(time.Second))
	obsolete := observe("schedule-obsolete", "flightaware_schedule", "arrived", now)
	alias(corrected.ID, obsolete.ID)
	local := observe("unrelated-local", "fixture", "arrived", now)
	provider := observe("schedule-unrelated", "flightaware_schedule", "arrived", now)
	alias(local.ID, provider.ID) // An unverified local row shares the provider key's spelling.
	track(owner, canonical, placeholder, downgrade, stale, distinct, corrected, obsolete, local, provider)
	track(other, canonical, obsolete)
	assertIDs := func(account string, want ...string) {
		t.Helper()
		flights, err := s.CompletedFlights(ctx, account)
		if err != nil {
			t.Fatal(err)
		}
		got := passportFlightIDs(flights)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("completed IDs = %v, want %v", got, want)
		}
	}
	assertIDs(owner, placeholder.ID, distinct.ID, local.ID, provider.ID)
	assertIDs(other, canonical.ID, obsolete.ID)
	first, after, err := s.CompletedFlightPage(ctx, owner, nil, 2)
	if err != nil || after == nil || after.ID != "p."+canonical.ID || !slices.Equal(passportRecordIDs(first), []string{local.ID, placeholder.ID}) {
		t.Fatalf("before alias change = %v, next = %+v, error = %v", passportRecordIDs(first), after, err)
	}
	observe(canonical.ID, canonical.Source, canonical.Status, placeholder.ObservedAt)
	assertIDs(owner, canonical.ID, distinct.ID, local.ID, provider.ID)
	last, after, err := s.CompletedFlightPage(ctx, owner, after, 2)
	if err != nil || after != nil || !slices.Equal(passportRecordIDs(last), []string{distinct.ID, provider.ID}) {
		t.Fatalf("after alias change = %v, next = %+v, error = %v", passportRecordIDs(last), after, err)
	}
}

func TestCompletedFlightPageKeepsTiesAndMissingTimesAcrossInserts(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "passport.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner, other := friendInstallation(t, s), friendInstallation(t, s)
	now := time.Now().UTC()
	arrival, older := now.Add(-time.Hour), now.Add(-2*time.Hour)
	add := func(account, id string, actual, scheduled *time.Time) {
		t.Helper()
		flight := feeds.Flight{ID: id, Status: "arrived", ObservedAt: now, ActualArrival: actual, ScheduledArrival: scheduled}
		if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.Track(ctx, account, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"tied-c", "tied-b", "tied-a"} {
		add(owner, id, &arrival, nil)
	}
	add(owner, "fallback", nil, &older)
	add(owner, "missing-b", nil, nil)
	add(owner, "missing-a", nil, nil)
	add(other, "foreign", &arrival, nil)
	var walked []string
	if err := s.WalkCompletedFlights(ctx, owner, func(flight feeds.Flight, cursor CompletedFlightCursor) {
		if cursor.ID != "l."+flight.ID || (cursor.Arrival == 0) != (flight.ActualArrival == nil && flight.ScheduledArrival == nil) {
			t.Errorf("unexpected cursor %+v for flight %s", cursor, flight.ID)
		}
		walked = append(walked, flight.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(walked, []string{"tied-a", "tied-b", "tied-c", "fallback", "missing-a", "missing-b"}) {
		t.Fatalf("walk order = %v", walked)
	}
	first, after, err := s.CompletedFlightPage(ctx, owner, nil, 2)
	if err != nil || after == nil || after.ID != "l.tied-b" || !slices.Equal(passportRecordIDs(first), []string{"tied-a", "tied-b"}) {
		t.Fatalf("first page = %v, next = %+v, error = %v", passportRecordIDs(first), after, err)
	}
	add(owner, "newer", &now, nil)
	if err := s.Untrack(ctx, owner, "tied-b"); err != nil {
		t.Fatal(err)
	}
	second, after, err := s.CompletedFlightPage(ctx, owner, after, 2)
	if err != nil || after == nil || after.ID != "l.fallback" || !slices.Equal(passportRecordIDs(second), []string{"tied-c", "fallback"}) {
		t.Fatalf("second page = %v, next = %+v, error = %v", passportRecordIDs(second), after, err)
	}
	third, after, err := s.CompletedFlightPage(ctx, owner, after, 1)
	if err != nil || after == nil || after.Arrival != 0 || !slices.Equal(passportRecordIDs(third), []string{"missing-a"}) {
		t.Fatalf("third page = %v, next = %+v, error = %v", passportRecordIDs(third), after, err)
	}
	last, after, err := s.CompletedFlightPage(ctx, owner, after, 1)
	if err != nil || after != nil || !slices.Equal(passportRecordIDs(last), []string{"missing-b"}) {
		t.Fatalf("last page = %v, next = %+v, error = %v", passportRecordIDs(last), after, err)
	}
}

func TestCompletedFlightPageFillsMissingCompatibleAliasMeasurements(t *testing.T) {
	for _, name := range []string{"compatible", "route conflict", "chronology conflict", "explicit facts"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			s, err := Open(filepath.Join(t.TempDir(), "passport.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			owner := friendInstallation(t, s)
			now := time.Now().UTC()
			arrival, departure, scheduled := now.Add(-time.Hour), now.Add(-3*time.Hour), now.Add(-70*time.Minute)
			older := feeds.Flight{ID: "older-alias", Source: "flightaware_schedule", Status: "arrived", ObservedAt: now.Add(-time.Minute),
				Origin: "LAX", OriginICAO: "KLAX", Destination: "SFO", DestinationICAO: "KSFO", TailNumber: "N1AA",
				ActualDeparture: &departure, ActualArrival: &arrival, ScheduledArrival: &scheduled, DistanceMiles: new(337)}
			flight := feeds.Flight{ID: "UAL1-canonical", Source: "flightaware_aeroapi", Status: "arrived", ObservedAt: now,
				Origin: "LAX", Destination: "SFO", ActualTakeoff: new(departure.Add(5 * time.Minute))}
			switch name {
			case "route conflict":
				flight.DestinationICAO = "KOAK"
			case "chronology conflict":
				flight.ActualLanding = new(arrival.Add(time.Minute))
			case "explicit facts":
				flight.ActualArrival, flight.DistanceMiles, flight.TailNumber = new(arrival.Add(time.Minute)), new(400), "N2AA"
			}
			for _, row := range []feeds.Flight{older, flight} {
				if err := s.Observe(ctx, row, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				if err := s.Track(ctx, owner, row.ID, ""); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO aero_instance_keys VALUES(?,?)`, flight.ID, older.ID); err != nil {
				t.Fatal(err)
			}
			rows, next, err := s.CompletedFlightPage(ctx, owner, nil, 1)
			if err != nil || next != nil || len(rows) != 1 || rows[0].Cursor.ID != "p."+flight.ID {
				t.Fatalf("completed page = %+v, next = %+v, error = %v", rows, next, err)
			}
			got := rows[0].Flight
			if got.ID != flight.ID || !got.ObservedAt.Equal(now) {
				t.Fatalf("older alias replaced current snapshot: %+v", got)
			}
			if name == "route conflict" {
				if got.DestinationICAO != "KOAK" || got.OriginICAO != "" || got.ActualArrival != nil || got.ScheduledArrival != nil ||
					got.ActualDeparture != nil || got.DistanceMiles != nil || got.TailNumber != "" {
					t.Fatalf("conflicting route supplied measurements: %+v", got)
				}
				return
			}
			if got.OriginICAO != older.OriginICAO || got.DestinationICAO != older.DestinationICAO ||
				got.ScheduledArrival == nil || !got.ScheduledArrival.Equal(scheduled) ||
				got.ActualDeparture == nil || !got.ActualDeparture.Equal(departure) {
				t.Fatalf("compatible older measurements missing: %+v", got)
			}
			if name == "chronology conflict" {
				if got.ActualArrival != nil {
					t.Fatalf("copied gate arrival before actual landing: %+v", got)
				}
			} else {
				wantArrival := arrival
				wantDistance, wantTail := 337, "N1AA"
				if name == "explicit facts" {
					wantArrival, wantDistance, wantTail = *flight.ActualArrival, *flight.DistanceMiles, flight.TailNumber
				}
				if got.ActualArrival == nil || !got.ActualArrival.Equal(wantArrival) || got.DistanceMiles == nil ||
					*got.DistanceMiles != wantDistance || got.TailNumber != wantTail {
					t.Fatalf("Passport measurements = %+v", got)
				}
			}
		})
	}
}

func passportFlightIDs(flights []feeds.Flight) []string {
	ids := make([]string, len(flights))
	for i, flight := range flights {
		ids[i] = flight.ID
	}
	return ids
}

func passportRecordIDs(flights []CompletedFlightRecord) []string {
	ids := make([]string, len(flights))
	for i, record := range flights {
		ids[i] = record.Flight.ID
	}
	return ids
}
