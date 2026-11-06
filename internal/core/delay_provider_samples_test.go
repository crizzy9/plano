package core

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

func TestProviderDelaySampleKeepsFirstForecastAndFencesOutcomes(t *testing.T) {
	store := openProviderDelayFixture(t)
	schedule := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	first := feeds.Flight{ID: "alias-one", Source: "flightaware_aeroapi", Status: "scheduled",
		ObservedAt: schedule.Add(-time.Hour), ScheduledDeparture: &schedule,
		EstimatedDeparture: new(schedule.Add(15 * time.Minute))}
	recordProviderDelayFixture(t, store, "provider-one", first, true)
	second := first
	second.ID, second.ObservedAt = "alias-two", first.ObservedAt.Add(10*time.Minute)
	second.EstimatedDeparture = new(schedule.Add(45 * time.Minute))
	recordProviderDelayFixture(t, store, "provider-one", second, true)
	forecast := readProviderDelayFixture(t, store, "provider-one")
	if forecast.observed != first.ObservedAt.UnixNano() || forecast.scheduled != schedule.UnixNano() || forecast.features.AirlineDelayMinutes != 15 {
		t.Fatalf("second alias changed the first forecast: %+v", forecast)
	}

	actual := schedule.Add(30 * time.Minute)
	label := actual.Add(time.Minute)
	second.ActualDeparture, second.Status, second.ObservedAt = &actual, "taxi_out", label
	recordProviderDelayFixture(t, store, "provider-one", second, false)
	second.ObservedAt = label.Add(time.Minute)
	recordProviderDelayFixture(t, store, "provider-one", second, false)
	if got := readProviderDelayFixture(t, store, "provider-one"); got.departed.Int64 != actual.UnixNano() ||
		got.label.Int64 != label.UnixNano() || got.outcome != second.ObservedAt.UnixNano() {
		t.Fatalf("repeated gate-out changed its first label time: %+v", got)
	}

	first.ObservedAt, first.Status = label.Add(3*time.Minute), "cancelled"
	recordProviderDelayFixture(t, store, "provider-one", first, false)
	recordProviderDelayFixture(t, store, "provider-one", second, false)
	second.ObservedAt = first.ObservedAt
	recordProviderDelayFixture(t, store, "provider-one", second, false)
	if got := readProviderDelayFixture(t, store, "provider-one"); got.departed.Valid || got.label.Valid || got.outcome != first.ObservedAt.UnixNano() {
		t.Fatalf("older or tied alias resurrected the cancelled label: %+v", got)
	}

	// A remap creates an independent forecast and outcome even with the same
	// local alias. It cannot change the original provider sample.
	replacementSchedule := schedule.Add(2 * time.Hour)
	first.Status, first.ScheduledDeparture, first.ObservedAt = "scheduled", &replacementSchedule, replacementSchedule.Add(-time.Hour)
	recordProviderDelayFixture(t, store, "provider-two", first, true)
	first.ActualDeparture, first.ObservedAt = new(replacementSchedule.Add(time.Minute)), replacementSchedule.Add(2*time.Minute)
	recordProviderDelayFixture(t, store, "provider-two", first, false)
	if got := readProviderDelayFixture(t, store, "provider-two"); got.scheduled != replacementSchedule.UnixNano() || got.departed.Int64 != first.ActualDeparture.UnixNano() {
		t.Fatalf("replacement instance did not receive its own outcome: %+v", got)
	}
	if got := readProviderDelayFixture(t, store, "provider-one"); got.departed.Valid {
		t.Fatalf("replacement outcome crossed the provider boundary: %+v", got)
	}
	second.ObservedAt, second.ActualDeparture = label.Add(5*time.Minute), new(actual.Add(time.Minute))
	recordProviderDelayFixture(t, store, "provider-one", second, false)
	if got := readProviderDelayFixture(t, store, "provider-one"); got.departed.Int64 != second.ActualDeparture.UnixNano() ||
		got.label.Int64 != second.ObservedAt.UnixNano() || got.observed != forecast.observed || got.features != forecast.features {
		t.Fatalf("valid later correction did not preserve the original forecast: %+v", got)
	}
}

func TestProviderDelaySampleAdoptsDirectLegacyAndRespectsAliasEvidence(t *testing.T) {
	store := openProviderDelayFixture(t)
	ctx := t.Context()
	schedule := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	firstObserved := schedule.Add(-time.Hour)
	actual := schedule.Add(15 * time.Minute)
	labelObserved := actual.Add(time.Minute)
	legacy := feeds.Flight{ID: "direct", Source: "flightaware_schedule", Status: "scheduled", ObservedAt: labelObserved.Add(time.Minute)}
	putProviderAliasFixture(t, store, "direct", legacy)
	alias := feeds.Flight{ID: "scheduled_unknown", Source: "flightaware_aeroapi", Status: "scheduled", ObservedAt: firstObserved}
	putProviderAliasFixture(t, store, "other", alias)
	for _, flight := range []feeds.Flight{legacy, alias} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO delay_samples(flight_id,observed_ns,scheduled_ns,features,departed_ns,label_observed_ns)
VALUES(?,?,?,'{"airlineDelayMinutes":20}',?,?)`, flight.ID, firstObserved.UnixNano(), schedule.UnixNano(), actual.UnixNano(), labelObserved.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	newer := feeds.Flight{ID: "cancelled-alias", Source: "flightaware_aeroapi", Status: "cancelled", ObservedAt: labelObserved.Add(3 * time.Minute)}
	putProviderAliasFixture(t, store, "direct", newer)
	older := feeds.Flight{ID: "stale-alias", Source: "flightaware_aeroapi", Status: "taxi_out",
		ObservedAt: labelObserved.Add(2 * time.Minute), ActualDeparture: &actual}
	recordProviderDelayFixture(t, store, "direct", older, false)
	got := readProviderDelayFixture(t, store, "direct")
	if got.observed != firstObserved.UnixNano() || got.scheduled != schedule.UnixNano() || got.features.AirlineDelayMinutes != 20 ||
		got.departed.Valid || got.label.Valid || got.outcome != newer.ObservedAt.UnixNano() {
		t.Fatalf("direct legacy adoption ignored stronger outcome evidence: %+v", got)
	}
	recordProviderDelayFixture(t, store, "other", older, false)
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delay_provider_samples WHERE provider_id='other'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("legacy schedule alias was adopted: %d, %v", count, err)
	}

	newer.ID, newer.ObservedAt = "newer-boarding-peer", firstObserved
	putProviderAliasFixture(t, store, "other", newer)
	older.Status, older.ActualDeparture, older.ObservedAt, older.ScheduledDeparture = "scheduled", nil, firstObserved, &schedule
	recordProviderDelayFixture(t, store, "other", older, true)
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delay_provider_samples WHERE provider_id='other'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("equal-time cancellation did not block a stale forecast capture: %d, %v", count, err)
	}
}

func openProviderDelayFixture(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func recordProviderDelayFixture(t *testing.T, store *Store, provider string, flight feeds.Flight, capture bool) {
	t.Helper()
	ctx := t.Context()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := recordProviderDelaySample(ctx, tx, provider, flight, nil, nil, flight.ObservedAt, capture); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func putProviderAliasFixture(t *testing.T, store *Store, provider string, flight feeds.Flight) {
	t.Helper()
	raw, err := json.Marshal(flight)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `INSERT INTO flights(id,snapshot,observed_ns) VALUES(?,?,?)`, flight.ID, string(raw), flight.ObservedAt.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `INSERT INTO aero_instance_keys(provider_id,flight_id) VALUES(?,?)`, provider, flight.ID); err != nil {
		t.Fatal(err)
	}
}

type providerDelayFixtureRow struct {
	observed, scheduled, outcome int64
	features                     predict.Features
	departed, label              sql.NullInt64
}

func readProviderDelayFixture(t *testing.T, store *Store, provider string) providerDelayFixtureRow {
	t.Helper()
	var row providerDelayFixtureRow
	var raw []byte
	if err := store.db.QueryRowContext(t.Context(), `SELECT observed_ns,scheduled_ns,features,departed_ns,label_observed_ns,outcome_observed_ns
FROM delay_provider_samples WHERE provider_id=?`, provider).Scan(&row.observed, &row.scheduled, &raw, &row.departed, &row.label, &row.outcome); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &row.features); err != nil {
		t.Fatal(err)
	}
	return row
}
