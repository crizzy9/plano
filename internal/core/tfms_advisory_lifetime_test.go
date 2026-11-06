package core

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestOpenExtendsLegacyTFMSProgramDeadlines(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy-tfms.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.tfmsLegacyReplayCutoff != 0 {
		t.Fatalf("fresh store has a replay cutoff: %d", store.tfmsLegacyReplayCutoff)
	}
	// The fixture's rows use the old schema and predate the lifetime marker.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM tfms_advisory_lifetime_policy WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	observed := now.Add(-10 * time.Hour)
	legacyExpiry := observed.Add(6 * time.Hour)
	program := func(airport string, ends time.Time) feeds.Advisory {
		return feeds.Advisory{
			Source: feeds.FAASWIM, ID: "tfms/gdp/" + airport, Airport: airport,
			Kind: "ground_delay", Direction: "arrival", AirportContextOnly: true,
			ObservedAt: observed, StartsAt: &observed, EndsAt: &ends,
		}
	}
	const latest = "pushed_advisories"
	const predecessor = "pushed_advisory_predecessors"
	publishedEnd := now.Add(3 * time.Hour)
	long := program("EWR", observed.Add(60*time.Hour))
	withdrawal := feeds.Advisory{
		Source: long.Source, ID: long.ID, Airport: long.Airport,
		Kind: "cancelled", ObservedAt: now.Add(20 * time.Second),
	}
	other := program("LAX", publishedEnd)
	other.ID = "different-feed-program"
	shortExpiry := legacyExpiry.Add(-time.Hour)
	rows := []struct {
		table          string
		advisory       feeds.Advisory
		oldEnd, newEnd time.Time
		payload        string
	}{
		{table: latest, advisory: program("ORD", publishedEnd), oldEnd: legacyExpiry, newEnd: publishedEnd},
		{table: latest, advisory: withdrawal, oldEnd: withdrawal.ObservedAt.Add(6 * time.Hour), newEnd: withdrawal.ObservedAt.Add(6 * time.Hour)},
		{table: predecessor, advisory: long, oldEnd: legacyExpiry, newEnd: observed.Add(feeds.TFMSAdvisoryMaxAge)},
		{table: latest, advisory: other, oldEnd: legacyExpiry, newEnd: legacyExpiry},
		{table: latest, advisory: program("SFO", publishedEnd), oldEnd: shortExpiry, newEnd: shortExpiry},
		{table: latest, advisory: program("JFK", now.Add(-time.Hour)), oldEnd: legacyExpiry, newEnd: legacyExpiry},
	}
	for i := range rows {
		row := &rows[i]
		raw, err := json.Marshal(row.advisory)
		if err != nil {
			t.Fatal(err)
		}
		row.payload = string(raw)
		if _, err := store.db.ExecContext(ctx, `INSERT INTO `+row.table+`(source,id,observed_ns,expires_ns,data) VALUES(?,?,?,?,?)`,
			row.advisory.Source, row.advisory.ID, row.advisory.ObservedAt.UnixNano(), row.oldEnd.UnixNano(), row.payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var migratedCutoff int64
	for _, name := range []string{"upgrade", "reopen"} {
		t.Run(name, func(t *testing.T) {
			firstCutoff := time.Now().Add(-signedAdvisoryRetention).UnixNano()
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if name == "upgrade" {
				migratedCutoff = reopened.tfmsLegacyReplayCutoff
				lastCutoff := time.Now().Add(-signedAdvisoryRetention).UnixNano()
				if migratedCutoff < firstCutoff || migratedCutoff > lastCutoff {
					t.Fatalf("legacy cutoff %d outside upgrade range [%d, %d]", migratedCutoff, firstCutoff, lastCutoff)
				}
			} else if reopened.tfmsLegacyReplayCutoff != migratedCutoff {
				t.Fatalf("legacy cutoff moved from %d to %d on reopen", migratedCutoff, reopened.tfmsLegacyReplayCutoff)
			}
			for _, row := range rows {
				var obs, expiry int64
				var payload string
				err := reopened.db.QueryRowContext(ctx, `SELECT observed_ns,expires_ns,data FROM `+row.table+` WHERE source=? AND id=?`,
					row.advisory.Source, row.advisory.ID).Scan(&obs, &expiry, &payload)
				if err != nil {
					t.Fatal(err)
				}
				if obs != row.advisory.ObservedAt.UnixNano() || payload != row.payload || expiry != row.newEnd.UnixNano() {
					t.Errorf("%s %s: observation %d, deadline %s, payload preserved %t; want deadline %s",
						row.table, row.advisory.ID, obs, time.Unix(0, expiry).UTC(), payload == row.payload, row.newEnd)
				}
			}
		})
	}
}
