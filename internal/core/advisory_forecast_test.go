package core

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

func TestAdvisoryForecastUsesEarliestApplicableValidity(t *testing.T) {
	now := time.Date(2026, time.November, 5, 20, 0, 0, 0, time.UTC)
	departure := now.Add(5 * time.Minute)
	flight := feeds.Flight{Origin: "SFO", Destination: "JFK", ScheduledDeparture: &departure}
	rows := []feeds.Advisory{
		{ID: "stop", Source: feeds.FAASWIM, Kind: "ground_stop", Airport: "JFK", Direction: "arrival", Reason: "ATC staffing", ObservedAt: now.Add(-time.Hour), EndsAt: new(now.Add(10 * time.Minute))},
		{ID: "undated", Kind: "departure_delay", Airport: "SFO", Direction: "departure", Reason: "weather", DelayMinutes: new(18), ObservedAt: now.Add(-25 * time.Minute)},
		{ID: "ended", Kind: "departure_delay", Airport: "SFO", Direction: "departure", Reason: "ended", DelayMinutes: new(99), ObservedAt: now.Add(-time.Hour), EndsAt: new(now)},
		{ID: "old", Kind: "departure_delay", Airport: "SFO", Direction: "departure", Reason: "too old", DelayMinutes: new(99), ObservedAt: now.Add(-advisoryForecastTTL)},
		{ID: "old-end", Kind: "departure_delay", Airport: "SFO", Direction: "departure", Reason: "too old", DelayMinutes: new(99), ObservedAt: now.Add(-maxNoticeAge), EndsAt: new(now.Add(time.Hour))},
		{ID: "future", Kind: "departure_delay", Airport: "SFO", Direction: "departure", Reason: "future", DelayMinutes: new(99), ObservedAt: now.Add(time.Second)},
		{ID: "unknown", Kind: "departure_delay", Airport: "SFO", Direction: "departure", Reason: "unknown time", DelayMinutes: new(99)},
		{ID: "withdrawn", Kind: "cancelled", Airport: "SFO", Reason: "cancelled", DelayMinutes: new(99), ObservedAt: now},
	}
	features, ok := ExtractDelayFeatures(flight, nil, rows, now)
	if !ok || features != (predict.Features{AirportDelayMinutes: 18, GroundStopMinutes: 5, Weather: true, AirTrafficControl: true}) {
		t.Fatalf("unexpected operational evidence: %+v %v", features, ok)
	}
	got := Predict(flight, nil, rows, now)
	if got == nil || got.DelayMinutes != 18 || got.ValidUntil == nil || !got.ValidUntil.Equal(now.Add(5*time.Minute)) ||
		len(got.Reasons) != 2 || len(got.AdvisoryEvidence) != 2 {
		t.Fatalf("unexpected estimate or expiry: %+v", got)
	}
}

func TestStoredAdvisoryForecastExpiresOrLosesItsRevision(t *testing.T) {
	for _, name := range []string{"expired", "legacy", "withdrawn"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			store, err := Open(filepath.Join(t.TempDir(), "forecast.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			now := time.Now().UTC()
			observed := now.Add(-5 * time.Minute)
			end := now.Add(15 * time.Minute)
			if name == "expired" {
				observed, end = now.Add(-40*time.Minute), now.Add(-time.Minute)
			}
			flight := feeds.Flight{ID: "delay", Origin: "SFO", ObservedAt: observed, ScheduledDeparture: new(observed.Add(time.Minute))}
			advisory := feeds.Advisory{ID: "observed-delay", Source: feeds.FAASWIM, Airport: "SFO", Kind: "departure_delay",
				Reason: "weather", DelayMinutes: new(35), ObservedAt: observed, EndsAt: &end}
			if err := store.Observe(ctx, flight, nil, nil, []feeds.Advisory{advisory}); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := store.db.QueryRowContext(ctx, `SELECT prediction FROM flights WHERE id=?`, flight.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var original *Prediction
			if err := json.Unmarshal(raw, &original); err != nil || original == nil || original.ValidUntil == nil || len(original.AdvisoryEvidence) != 1 {
				t.Fatalf("did not save forecast evidence: %+v %v", original, err)
			}
			if name != "expired" {
				details, err := store.Details(ctx, flight.ID)
				if err != nil || details.Prediction == nil || len(details.Prediction.AdvisoryEvidence) != 0 {
					t.Fatalf("active forecast was hidden or leaked its internal references: %+v %v", details.Prediction, err)
				}
			}
			if name == "legacy" {
				if _, err := store.db.ExecContext(ctx, `UPDATE flights SET prediction=json_remove(prediction,'$.validUntil','$.advisoryEvidence') WHERE id=?`, flight.ID); err != nil {
					t.Fatal(err)
				}
			}
			if name == "withdrawn" {
				advisory.Kind, advisory.ObservedAt = "cancelled", now.Add(-time.Second)
				if err := store.PublishAdvisory(ctx, advisory, now); err != nil {
					t.Fatal(err)
				}
			}
			if details, err := store.Details(ctx, flight.ID); err != nil || details.Prediction != nil {
				t.Fatalf("obsolete forecast remained visible: %+v %v", details.Prediction, err)
			}
			flight.ObservedAt = now
			if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := store.db.QueryRowContext(ctx, `SELECT prediction FROM flights WHERE id=?`, flight.ID).Scan(&raw); err != nil || string(raw) != "null" {
				t.Fatalf("obsolete forecast survived a flight-only refresh: %s %v", raw, err)
			}
		})
	}
}

func TestLearnedReasonNamesAnObservedSignal(t *testing.T) {
	contributions := []predict.Contribution{{Feature: "weather", Minutes: 10}, {Feature: "ground_stop", Minutes: 8}, {Feature: "airline_delay", Minutes: 4}}
	if reason := learnedReason(predict.Features{AirlineDelayMinutes: 4}, contributions); !strings.Contains(reason, "airline's estimate") {
		t.Fatalf("an absent feature was named as current evidence: %s", reason)
	}
	if reason := learnedReason(predict.Features{AirlineDelayMinutes: 4, Weather: true}, contributions); !strings.Contains(reason, "weather reports") {
		t.Fatalf("the strongest observed feature was omitted: %s", reason)
	}
}

func TestAdvisoryEventRequiresARelevantCurrentOperation(t *testing.T) {
	now := time.Date(2026, time.November, 5, 20, 0, 0, 0, time.UTC)
	flight := feeds.Flight{Origin: "SFO", Destination: "JFK", ScheduledDeparture: new(now.Add(-40 * time.Minute)), ScheduledArrival: new(now.Add(15 * time.Minute))}
	advisory := feeds.Advisory{ID: "stop", Kind: "ground_stop", Direction: "arrival", Airport: "JFK", ObservedAt: now.Add(-time.Minute), EndsAt: new(now.Add(10 * time.Minute))}
	if event, ok := advisoryEvent(flight, advisory, now); !ok || event.validUntil == nil || !event.validUntil.Equal(*advisory.EndsAt) {
		t.Fatalf("matching hold lost its validity: %+v %v", event, ok)
	}
	ended := advisory
	ended.EndsAt = new(now.Add(-10 * time.Minute))
	if _, ok := advisoryEvent(flight, ended, now); ok {
		t.Fatal("ended advisory produced an alert")
	}
	withdrawn := advisory
	withdrawn.Kind, withdrawn.EndsAt = "cancelled", nil
	if _, ok := advisoryEvent(flight, withdrawn, now); ok {
		t.Fatal("withdrawal produced an active advisory alert")
	}
	flight.Status = "airborne"
	if _, ok := advisoryEvent(flight, advisory, now); ok {
		t.Fatal("ground stop was sent for an airborne flight")
	}
	advisory.Kind, advisory.EndsAt = "arrival_delay", nil
	if _, ok := advisoryEvent(flight, advisory, now); !ok {
		t.Fatal("airborne flight lost a valid destination arrival alert")
	}
}
