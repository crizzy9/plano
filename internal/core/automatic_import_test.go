package core

import (
	"testing"
	"time"
)

func TestAutomaticImportSourcesShareFlightReceipts(t *testing.T) {
	for _, first := range []string{"calendar", "tripit"} {
		t.Run(first, func(t *testing.T) {
			store, owner, flightID := dispatcherFixture(t)
			ctx := t.Context()
			now := time.Now().UTC()
			date := now.AddDate(0, 0, 1).Format(time.DateOnly)
			calendar := func() {
				t.Helper()
				leg := CalendarImportLeg{Key: date + " UA123", FlightID: flightID, ExpiresAt: now.AddDate(0, 0, 4)}
				if err := store.CommitCalendarImport(ctx, owner, []CalendarImportLeg{leg}); err != nil {
					t.Fatal(err)
				}
				if completed, err := store.CalendarImportCompleted(ctx, owner, leg.Key); err != nil || !completed {
					t.Fatalf("calendar receipt = %t, error = %v", completed, err)
				}
			}
			tripit := func() {
				t.Helper()
				if err := store.SetTripItSubscription(ctx, owner, []byte("fixture-encrypted-url"), now); err != nil {
					t.Fatal(err)
				}
				job, err := store.ClaimTripItSync(ctx, now)
				if err != nil || job == nil {
					t.Fatalf("TripIt job = %v, error = %v", job, err)
				}
				match := TripItMatch{Key: "fixture-tripit-leg", Date: date, FlightID: flightID}
				if err := store.FinishTripItSync(ctx, *job, TripItSyncResult{Matches: []TripItMatch{match}, Matched: 1}, now, now.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				seen, err := store.TripItImportedLegs(ctx, owner, date)
				if err != nil || !seen[match.Key] {
					t.Fatalf("TripIt receipt = %t, error = %v", seen[match.Key], err)
				}
			}
			initial, second := calendar, tripit
			if first == "tripit" {
				initial, second = tripit, calendar
			}
			assertTrackedCount := func(want int) {
				t.Helper()
				tracked, err := store.Tracked(ctx, owner)
				if err != nil || len(tracked) != want {
					t.Fatalf("tracked = %v, want %d flights, error = %v", tracked, want, err)
				}
			}
			initial()
			assertTrackedCount(1)
			if err := store.Untrack(ctx, owner, flightID); err != nil {
				t.Fatal(err)
			}
			second()
			assertTrackedCount(0)
			if err := store.Track(ctx, owner, flightID, "12A"); err != nil {
				t.Fatal(err)
			}
			assertTrackedCount(1)
		})
	}
}

func TestPruneExpiresReceiptsWithoutAnActiveSubscription(t *testing.T) {
	store, owner, _ := dispatcherFixture(t)
	ctx := t.Context()
	now := time.Now().UTC()
	oldDate, today := now.AddDate(0, 0, -3).Format(time.DateOnly), now.Format(time.DateOnly)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tripit_imported_legs(owner,leg_key,date) VALUES(?,?,?),(?,?,?)`,
		owner, "old", oldDate, owner, "current", today); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO automatic_flight_receipts(owner,digest,expires) VALUES(?,?,?),(?,?,?)`,
		owner, "expired", now.Unix(), owner, "future", now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
	seen, err := store.TripItImportedLegs(ctx, owner, oldDate)
	if err != nil || len(seen) != 1 || !seen["current"] {
		t.Fatalf("remaining TripIt receipts = %v, error = %v", seen, err)
	}
	var count int
	var remaining string
	if err := store.db.QueryRowContext(ctx, `SELECT count(*),MIN(digest) FROM automatic_flight_receipts WHERE owner=?`, owner).
		Scan(&count, &remaining); err != nil || count != 1 || remaining != "future" {
		t.Fatalf("remaining flight receipts = %d, key = %q, error = %v", count, remaining, err)
	}
}
