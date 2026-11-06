package core

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestExpiredClosurePredictionIsHiddenAndNotPreservedDuringOutage(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "forecast.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	now := time.Now().UTC()
	observed := now.Add(-45 * time.Minute)
	flight := feeds.Flight{ID: "closure-forecast", Number: "UA123", Origin: "SFO", OriginICAO: "KSFO", Destination: "EWR",
		Status: "scheduled", ScheduledDeparture: new(now.Add(-40 * time.Minute)), ObservedAt: observed}
	closure := feeds.Advisory{ID: "full-closure", Source: feeds.FAASWIM, Kind: "airport_closure", Airport: "KSFO",
		RawReason: "!SFO SFO AD AP CLSD", StartsAt: new(observed.Add(-time.Minute)), EndsAt: new(now.Add(time.Hour)), ObservedAt: observed}
	if err := store.Observe(ctx, flight, nil, nil, []feeds.Advisory{closure}); err != nil {
		t.Fatal(err)
	}
	readPrediction := func() *Prediction {
		t.Helper()
		var raw []byte
		if err := store.db.QueryRowContext(ctx, `SELECT prediction FROM flights WHERE id=?`, flight.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var prediction *Prediction
		if err := json.Unmarshal(raw, &prediction); err != nil {
			t.Fatal(err)
		}
		return prediction
	}
	if prior := readPrediction(); prior == nil || prior.ValidUntil == nil || !prior.ValidUntil.Before(now) {
		t.Fatalf("expected an expired persisted closure forecast: %+v", prior)
	}
	if details, err := store.Details(ctx, flight.ID); err != nil || details.Prediction != nil {
		t.Fatalf("expired forecast returned from details: %+v, %v", details.Prediction, err)
	}
	flight.ObservedAt = now
	if err := store.observeTracked(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if prior := readPrediction(); prior != nil {
		t.Fatalf("expired closure forecast survived missing advisories: %+v", prior)
	}
}
