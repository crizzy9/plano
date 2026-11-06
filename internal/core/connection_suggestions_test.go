package core

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestConnectionSuggestionPoolKeepsMatchingArrival(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "plano.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	token, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC)
	// Earlier arrivals in the 24-hour window share the onward display code.
	// Half have a different verified airport and half require booking evidence.
	_, err = store.db.ExecContext(ctx, `
WITH RECURSIVE sequence(n) AS (
  VALUES(1) UNION ALL SELECT n+1 FROM sequence WHERE n<300
)
INSERT INTO flights(id,snapshot,observed_ns)
SELECT 'earlier-'||n, json_object('id','earlier-'||n,'destination','DEN',
  'destinationICAO','KBJC','scheduledArrival',:now), :observed FROM sequence
UNION ALL SELECT 'long-'||n, json_object('id','long-'||n,'destination','DEN',
  'destinationICAO','KDEN','scheduledArrival',:long_arrival), :observed FROM sequence
UNION ALL SELECT 'pair-in', json_object('id','pair-in','destination','DEN',
  'destinationICAO','KDEN','scheduledArrival',:arrival), :observed
UNION ALL SELECT 'pair-out', json_object('id','pair-out','origin','DEN',
  'originICAO','KDEN','scheduledDeparture',:departure), :observed`,
		sql.Named("now", now.Format(time.RFC3339)), sql.Named("observed", now.UnixNano()),
		sql.Named("long_arrival", now.Add(-12*time.Hour).Format(time.RFC3339)),
		sql.Named("arrival", now.Add(time.Hour).Format(time.RFC3339)),
		sql.Named("departure", now.Add(2*time.Hour).Format(time.RFC3339)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tracked(owner,flight_id) SELECT ?,id FROM flights`, owner); err != nil {
		t.Fatal(err)
	}
	flights, err := store.ConnectionSuggestionFlights(ctx, owner, now, now.Add(7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, flight := range flights {
		ids = append(ids, flight.Flight.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"pair-in", "pair-out"}) {
		t.Fatalf("unexpected suggestion pool: %v", ids)
	}
}
