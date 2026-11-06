package core

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

func TestResolvedScheduleAliasTracksIndependentDelaySamplesAfterRemap(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "provider-alias.db")
	store, err := OpenWithOptions(path, StoreOptions{CollectDelaySamples: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	provider := "UAL123-fixture-a"
	firstAt := now.Add(-30 * time.Second)
	scheduled := firstAt.Add(delaySampleTarget(provider, standardDelaySampleTables))
	alias := "scheduled_" + base64.RawURLEncoding.EncodeToString([]byte("UA123|UAL123|KSFO|KEWR|"+scheduled.Format(time.RFC3339)))
	placeholder := feeds.Flight{ID: alias, Number: "UA123", Source: "flightaware_schedule", Status: "scheduled",
		Origin: "SFO", Destination: "EWR", ScheduledDeparture: &scheduled, ObservedAt: firstAt.Add(-time.Minute)}
	if err := store.observeTracked(ctx, placeholder, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var unverified int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delay_samples`).Scan(&unverified); err != nil || unverified != 0 {
		t.Fatalf("unverified timetable captured %d examples, error %v", unverified, err)
	}
	estimate := scheduled.Add(10 * time.Minute)
	const inboundID = "UAL456-fixture"
	source := feeds.NewAeroAPI("fixture-only", &http.Client{Transport: aeroCallbackTransport(func(*http.Request) (*http.Response, error) {
		raw := fmt.Sprintf(`{"flights":[{"fa_flight_id":%q,"ident":"UAL123","ident_iata":"UA123","ident_icao":"UAL123","origin":{"code_icao":"KSFO","code_iata":"SFO"},"destination":{"code_icao":"KEWR","code_iata":"EWR"},"scheduled_out":%q,"estimated_out":%q,"registration":"N123AA","inbound_fa_flight_id":%q}]}`,
			provider, scheduled.Format(time.RFC3339), estimate.Format(time.RFC3339), inboundID)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}, nil
	})})
	get := func(at time.Time) feeds.Flight {
		t.Helper()
		flight, err := source.Get(ctx, alias)
		if err != nil || flight.ID != alias || flight.AeroProviderID() != provider {
			t.Fatalf("resolved flight = %+v, error %v", flight, err)
		}
		flight.ObservedAt = at
		return flight
	}
	first := get(firstAt)
	priorInbound := feeds.Flight{ID: inboundID, Number: "UA456", Status: "airborne", Destination: "SFO",
		TailNumber: "N123AA", ObservedAt: firstAt, ScheduledArrival: new(scheduled.Add(-2 * time.Minute)),
		EstimatedArrival: new(scheduled.Add(-time.Minute))}
	if err := store.observeTracked(ctx, first, nil, []feeds.Flight{priorInbound}, nil); err != nil {
		t.Fatal(err)
	}
	if before, err := store.Details(ctx, alias); err != nil || before.Prediction == nil || len(before.Inbound) != 1 {
		t.Fatalf("missing initial forecast and rotation: %+v, %v", before, err)
	}
	direct := first
	direct.ID, direct.ObservedAt = first.AeroProviderID(), firstAt.Add(5*time.Second)
	direct.EstimatedDeparture = new(scheduled.Add(35 * time.Minute))
	if err := store.observeTracked(ctx, direct, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	provider = "UAL123-fixture-b"
	estimate = scheduled.Add(25 * time.Minute)
	second := get(firstAt.Add(10 * time.Second))
	if err := store.Observe(ctx, second, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if after, err := store.Details(ctx, alias); err != nil || after.Prediction != nil || len(after.Inbound) != 0 {
		t.Fatalf("remapped flight inherited forecast or rotation: %+v, %v", after, err)
	}
	second.ObservedAt = second.ObservedAt.Add(time.Second)
	if captureAt := scheduled.Add(-delaySampleTarget(provider, standardDelaySampleTables)); captureAt.After(second.ObservedAt) {
		second.ObservedAt = captureAt
	}
	if err := store.observeTracked(ctx, second, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	second.Status, second.ActualDeparture, second.ObservedAt = "taxi_out", new(scheduled.Add(20*time.Minute)), scheduled.Add(22*time.Minute)
	if err := store.Observe(ctx, second, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	export := func() []predict.Example {
		t.Helper()
		var data bytes.Buffer
		if err := ExportDelaySamples(ctx, path, &data); err != nil {
			t.Fatal(err)
		}
		rows, err := predict.ReadExamples(&data)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if rows := export(); len(rows) != 1 || rows[0].FlightID != hash("delay-sample\x00"+provider) ||
		rows[0].Features.AirlineDelayMinutes != 25 || !rows[0].DepartedAt.Equal(*second.ActualDeparture) {
		t.Fatalf("replacement gate-out was misattributed: %+v", rows)
	}
	direct.Status, direct.ActualDeparture, direct.ObservedAt = "taxi_out", new(scheduled.Add(12*time.Minute)), scheduled.Add(24*time.Minute)
	if err := store.Observe(ctx, direct, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if rows := export(); len(rows) != 2 || rows[0].FlightID != hash("delay-sample\x00"+first.AeroProviderID()) ||
		!rows[0].ObservedAt.Equal(first.ObservedAt) || rows[0].Features.AirlineDelayMinutes != 10 ||
		!rows[0].DepartedAt.Equal(*direct.ActualDeparture) {
		t.Fatalf("provider aliases did not share the first verified example: %+v", rows)
	}
}

func TestExportDelaySamplesFromPreUpgradeBackup(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "pre-upgrade.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE flights(id TEXT PRIMARY KEY,snapshot TEXT,observed_ns INTEGER);
CREATE TABLE aero_instance_keys(provider_id TEXT,flight_id TEXT);
CREATE TABLE delay_samples(flight_id TEXT PRIMARY KEY,observed_ns INTEGER,scheduled_ns INTEGER,features TEXT,departed_ns INTEGER,label_observed_ns INTEGER);`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	departed := now.Add(-time.Minute)
	const provider = "UAL123-legacy"
	for _, local := range []string{provider, "scheduled_legacy"} {
		flight := feeds.Flight{ID: local, Source: "flightaware_aeroapi", Status: "taxi_out", ObservedAt: now, ActualDeparture: &departed}
		raw, err := json.Marshal(flight)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO flights VALUES(?,?,?);`, local, string(raw), now.UnixNano()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO aero_instance_keys VALUES(?,?)`, provider, local); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO delay_samples VALUES(?,?,?,?,?,?)`, local, now.Add(-time.Hour).UnixNano(),
			now.Add(-10*time.Minute).UnixNano(), "{}", departed.UnixNano(), now.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	var data bytes.Buffer
	if err := ExportEarlyDelaySamples(ctx, path, &data); err != nil || data.Len() != 0 {
		t.Fatalf("pre-upgrade backup early export = %q, %v", data.String(), err)
	}
	if err := ExportDelaySamples(ctx, path, &data); err != nil {
		t.Fatal(err)
	}
	rows, err := predict.ReadExamples(&data)
	if err != nil || len(rows) != 1 || rows[0].FlightID != hash("delay-sample\x00"+provider) {
		t.Fatalf("legacy direct history or alias handling changed: %+v, %v", rows, err)
	}
	cancel := feeds.Flight{ID: "scheduled_legacy", Source: "flightaware_aeroapi", Status: "cancelled", ObservedAt: now.Add(time.Second)}
	raw, err := json.Marshal(cancel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE flights SET snapshot=?,observed_ns=? WHERE id=?`, string(raw), cancel.ObservedAt.UnixNano(), cancel.ID); err != nil {
		t.Fatal(err)
	}
	data.Reset()
	if err := ExportDelaySamples(ctx, path, &data); err != nil || data.Len() != 0 {
		t.Fatalf("legacy export ignored the latest cancellation on a verified alias: %v, %q", err, data.String())
	}
}
