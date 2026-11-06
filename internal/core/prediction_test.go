package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

func TestPredictionFeaturesUseMatchingAndKnownSignals(t *testing.T) {
	now := time.Date(2026, time.November, 5, 17, 30, 0, 0, time.UTC)
	departure := now.Add(30 * time.Minute)
	arrival := departure.Add(4 * time.Hour)
	published := departure.Add(10 * time.Minute)
	flight := feeds.Flight{ID: "main", Number: "UA7", Status: "scheduled", Origin: "SFO", Destination: "JFK", OriginICAO: "KSFO", DestinationICAO: "KJFK",
		ScheduledDeparture: &departure, ScheduledArrival: &arrival, EstimatedDeparture: &published, InboundID: "inbound"}
	inbound := []feeds.Flight{{ID: "inbound", Number: "UA8", Status: "airborne", DestinationICAO: "KSFO", Destination: "KSFO",
		ObservedAt: now, ScheduledArrival: new(departure.Add(-20 * time.Minute)), EstimatedArrival: new(departure.Add(15 * time.Minute))}}
	stopEnd := departure.Add(70 * time.Minute)
	advisories := []feeds.Advisory{
		{Airport: "KJFK", Direction: "arrival", Kind: "ground_stop", Reason: "weather and ATC staffing", Source: "faa_nas", ObservedAt: now, EndsAt: &stopEnd},
		{Airport: "KSFO", Direction: "departure", Kind: "departure_delay", Reason: "airport volume", Source: "faa_nas", ObservedAt: now, DelayMinutes: new(40)},
		{Airport: "KSFO", Direction: "arrival", Kind: "arrival_delay", Reason: "unrelated direction", Source: "faa_nas", ObservedAt: now, DelayMinutes: new(100)},
		{Airport: "JFK", Direction: "arrival", Kind: "airport_restriction", Reason: "general aviation only", Source: "faa_nas", ObservedAt: now, DelayMinutes: new(90)},
		{Airport: "JFK", Direction: "arrival", Kind: "arrival_delay", Reason: "future report", Source: "faa_nas", ObservedAt: now.Add(time.Minute), DelayMinutes: new(110)},
	}
	features, ok := ExtractDelayFeatures(flight, inbound, advisories, now)
	want := predict.Features{AirlineDelayMinutes: 10, InboundDelayMinutes: 55, AirportDelayMinutes: 40, GroundStopMinutes: 70, Weather: true, AirTrafficControl: true}
	if !ok || features != want {
		t.Fatalf("features = %+v, %t, want %+v", features, ok, want)
	}
	forecast := Predict(flight, inbound, advisories, now)
	if forecast == nil || forecast.Method != "operational_estimate" || forecast.DelayMinutes != 70 || !forecast.Departure.Equal(stopEnd) || len(forecast.Reasons) != 3 {
		t.Fatalf("operational forecast = %+v", forecast)
	}
	if !strings.Contains(forecast.Reasons[0], "UA8") || !strings.Contains(forecast.Reasons[1], "FAA") || !flight.EstimatedDeparture.Equal(published) {
		t.Fatalf("published estimate or reason changed: %v, %+v", flight.EstimatedDeparture, forecast.Reasons)
	}
}

func TestPredictionUsesTrainedModelAndRetainsOperationalFallback(t *testing.T) {
	start := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]predict.Example, 40)
	for i := range rows {
		observed := start.Add(time.Duration(i) * 2 * time.Hour)
		departure := observed.Add(time.Hour)
		airlineDelay := 5 * (1 + i%5)
		rows[i] = predict.Example{
			FlightID: fmt.Sprintf("synthetic-%d", i), ObservedAt: observed, ScheduledDeparture: departure,
			DepartedAt: departure.Add(time.Duration(10+2*airlineDelay) * time.Minute),
			Features:   predict.Features{AirlineDelayMinutes: float64(airlineDelay)},
		}
	}
	model, _, err := predict.Fit(rows, predict.Options{HoldoutFraction: 0.2, Ridge: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	row := rows[39]
	published := row.ScheduledDeparture.Add(25 * time.Minute)
	flight := feeds.Flight{ID: row.FlightID, Number: "UA7", Status: "scheduled", Origin: "SFO", Destination: "JFK",
		ScheduledDeparture: &row.ScheduledDeparture, EstimatedDeparture: &published}
	got := PredictWithModel(model, flight, nil, nil, row.ObservedAt)
	if got == nil || got.Method != "learned_estimate" || got.DelayMinutes != 60 || got.ModelVersion != model.ID() || got.ValidUntil != nil || got.BaselineMinutes == nil || len(got.FeatureContributions) < 7 || len(got.Reasons) == 0 {
		t.Fatalf("learned forecast = %+v", got)
	}
	if !flight.EstimatedDeparture.Equal(published) || !flight.ScheduledDeparture.Equal(row.ScheduledDeparture) {
		t.Fatal("forecast modified published flight times")
	}
	closureEnd := row.ScheduledDeparture.Add(100 * time.Minute)
	closure := feeds.Advisory{Airport: "SFO", Direction: "departure", Kind: "airport_closure", Source: "faa_nas", RawReason: "AD AP CLSD",
		ObservedAt: row.ObservedAt, StartsAt: new(row.ScheduledDeparture.Add(45 * time.Minute)), EndsAt: &closureEnd}
	closed := PredictWithModel(model, flight, nil, []feeds.Advisory{closure}, row.ObservedAt)
	if closed == nil || closed.Method != got.Method || closed.ModelVersion != got.ModelVersion || closed.DelayMinutes != 100 ||
		!closed.Departure.Equal(closureEnd) || !slices.Equal(closed.FeatureContributions, got.FeatureContributions) ||
		closed.ValidUntil == nil || !closed.ValidUntil.Equal(row.ObservedAt.Add(30*time.Minute)) ||
		len(closed.Reasons) == 0 || !strings.Contains(closed.Reasons[len(closed.Reasons)-1], "full airport closure") {
		t.Fatalf("closure-adjusted learned forecast = %+v", closed)
	}
	advisory := feeds.Advisory{Airport: "SFO", Direction: "departure", Kind: "departure_delay", Reason: "weather", Source: "faa_nas", DelayMinutes: new(35), ObservedAt: row.ObservedAt}
	got = PredictWithModel(model, flight, nil, []feeds.Advisory{advisory}, row.ObservedAt)
	if got == nil || got.Method != "operational_estimate" || got.DelayMinutes != 35 || got.ModelVersion != "" {
		t.Fatalf("fallback forecast = %+v", got)
	}
	flight.Status = "airborne"
	if got := PredictWithModel(model, flight, nil, nil, row.ObservedAt); got != nil {
		t.Fatalf("predicted already departed flight: %+v", got)
	}
}

func TestPredictionUsesEarliestExpiryAcrossClosureEvidence(t *testing.T) {
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	departure := now.Add(5 * time.Minute)
	flight := feeds.Flight{Status: "scheduled", Origin: "SFO", ScheduledDeparture: &departure}
	first := feeds.Advisory{Airport: "SFO", Kind: "airport_closure", Source: "faa_nas", RawReason: "AD AP CLSD",
		ObservedAt: now, EndsAt: new(now.Add(20 * time.Minute))}
	second := feeds.Advisory{Airport: "SFO", Kind: "airport_closure", Source: "faa_nas", RawReason: "AD AP CLSD",
		ObservedAt: now, StartsAt: new(now.Add(15 * time.Minute)), EndsAt: new(now.Add(90 * time.Minute))}
	aged := first
	aged.ObservedAt = now.Add(-5*time.Hour - 50*time.Minute)
	for _, tc := range []struct {
		name     string
		first    feeds.Advisory
		validFor time.Duration
	}{
		{name: "earlier notice end", first: first, validFor: 20 * time.Minute},
		{name: "earlier observation expiry", first: aged, validFor: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Predict(flight, nil, []feeds.Advisory{second, tc.first}, now)
			if got == nil || !got.Departure.Equal(*second.EndsAt) || got.ValidUntil == nil || !got.ValidUntil.Equal(now.Add(tc.validFor)) {
				t.Fatalf("chained closure forecast = %+v", got)
			}
		})
	}
}

func TestPredictionUsesOverlappingFullDepartureClosures(t *testing.T) {
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	departure := now.Add(time.Hour)
	published := departure.Add(5 * time.Minute)
	flight := feeds.Flight{Status: "scheduled", Origin: "SFO", OriginICAO: "KSFO", Destination: "JFK", DestinationICAO: "KJFK",
		ScheduledDeparture: &departure, EstimatedDeparture: &published}
	end := departure.Add(75*time.Minute + 30*time.Second)
	advisories := []feeds.Advisory{
		{Airport: "KSFO", Kind: "airport_closure", Direction: "departure", Source: "faa_nas", RawReason: "!SFO SFO AD AP CLSD",
			ObservedAt: now, StartsAt: new(departure.Add(30 * time.Minute)), EndsAt: &end},
		{Airport: "SFO", Kind: "airport_closure", Source: "faa_nas", RawReason: "AD AP CLSD",
			ObservedAt: now, EndsAt: new(departure.Add(40 * time.Minute)), Reason: "weather and ATC", DelayMinutes: new(100)},
	}
	got := Predict(flight, nil, advisories, now)
	if got == nil || got.Method != "operational_estimate" || got.DelayMinutes != 76 || !got.Departure.Equal(end) ||
		len(got.Reasons) != 1 || !strings.Contains(got.Reasons[0], "FAA reports a full airport closure at KSFO") {
		t.Fatalf("closure-adjusted forecast = %+v", got)
	}
	features, ok := ExtractDelayFeatures(flight, nil, advisories, now)
	if !ok || features != (predict.Features{AirlineDelayMinutes: 5}) {
		t.Fatalf("closure entered the model features: %+v, %t", features, ok)
	}
	if !flight.EstimatedDeparture.Equal(published) || !flight.ScheduledDeparture.Equal(departure) {
		t.Fatal("closure forecast modified published flight times")
	}
}

func TestPredictionChecksPublishedNOTAMWindow(t *testing.T) {
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	start, end := now.Add(30*time.Minute), now.Add(2*time.Hour)
	flight := feeds.Flight{Status: "scheduled", Origin: "SFO", OriginICAO: "KSFO", ScheduledDeparture: new(now.Add(time.Hour))}
	const icao = "(A1234/26 NOTAMN\nA) KSFO B) 2611051830 C) 2611052000\nE) AD CLSD)"
	for _, tc := range []struct {
		name, raw  string
		start, end time.Time
		wantFloor  bool
	}{
		{"matching ICAO window", icao, start, end, true},
		{"matching domestic window", "!SFO SFO AD AP CLSD 2611051830-2611052000", start, end, true},
		{"legacy structured window", "AD CLSD", start, end, true},
		{"conflicting start", icao, start.Add(15 * time.Minute), end, false},
		{"conflicting end", icao, start, end.Add(15 * time.Minute), false},
		{"missing published end", "(A1234/26 NOTAMN\nA) KSFO B) 2611051830\nE) AD CLSD)", start, end, false},
		{"invalid published end", "(A1234/26 NOTAMN\nA) KSFO B) 2611051830 C) 2611312000\nE) AD CLSD)", start, end, false},
		{"estimated published end", "(A1234/26 NOTAMN\nA) KSFO B) 2611051830 C) 2611052000EST\nE) AD CLSD)", start, end, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			advisory := feeds.Advisory{Airport: "KSFO", Kind: "airport_closure", Source: feeds.FAASWIM,
				RawReason: tc.raw, ObservedAt: now, StartsAt: &tc.start, EndsAt: &tc.end}
			got := Predict(flight, nil, []feeds.Advisory{advisory}, now)
			if tc.wantFloor {
				if got == nil || !got.Departure.Equal(end) {
					t.Fatalf("forecast = %+v, want departure at %s", got, end)
				}
			} else if got != nil {
				t.Fatalf("uncertain published window changed the forecast: %+v", got)
			}
		})
	}
}

func TestPredictionFromBridgeNOTAMWindow(t *testing.T) {
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	end := now.Add(2 * time.Hour)
	flight := feeds.Flight{Status: "scheduled", Origin: "SFO", OriginICAO: "KSFO", ScheduledDeparture: new(now.Add(time.Hour))}
	for _, tc := range []struct {
		name, window string
		wantFloor    bool
	}{
		{"fixed window", "B) 2611051830 C) 2611052000", true},
		{"partial window", "B) 2611051830", false},
		{"estimated window", "B) 2611051830 C) 2611052000 EST", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "(A1234/26 NOTAMN\nA) KSFO " + tc.window + "\nE) AD CLSD)"
			body := fmt.Sprintf(`{"version":1,"source":"faa_swim","advisory":{"id":"fixture","airport":"KSFO","kind":"airport_closure","observedAt":%q,"rawReason":%q}}`,
				now.Format(time.RFC3339), raw)
			advisory, err := feeds.DecodeAdvisoryIngress(strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantFloor && (advisory.StartsAt == nil || !advisory.StartsAt.Equal(now.Add(30*time.Minute)) ||
				advisory.EndsAt == nil || !advisory.EndsAt.Equal(end)) {
				t.Fatalf("bridge did not fill the published window: %+v", advisory)
			}
			if !tc.wantFloor && (advisory.StartsAt != nil || advisory.EndsAt != nil) {
				t.Fatalf("bridge filled an uncertain published window: %+v", advisory)
			}
			got := Predict(flight, nil, []feeds.Advisory{advisory}, now)
			if tc.wantFloor {
				if got == nil || !got.Departure.Equal(end) {
					t.Fatalf("bridge forecast = %+v, want departure at %s", got, end)
				}
			} else if got != nil {
				t.Fatalf("uncertain bridge window changed the forecast: %+v", got)
			}
		})
	}
}

func TestPredictionLeavesAmbiguousAndUnrelatedNoticesAsContext(t *testing.T) {
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	departure := now.Add(time.Hour)
	flight := feeds.Flight{Status: "scheduled", Origin: "SFO", OriginICAO: "KSFO", Destination: "JFK", DestinationICAO: "KJFK",
		ScheduledDeparture: &departure}
	valid := feeds.Advisory{Airport: "KSFO", Kind: "airport_closure", Source: "faa_nas", RawReason: "AD AP CLSD",
		ObservedAt: now, EndsAt: new(departure.Add(time.Hour)), Reason: "weather and ATC", DelayMinutes: new(60)}
	for _, tc := range []struct {
		name   string
		change func(*feeds.Advisory)
	}{
		{"arrival-only at origin", func(a *feeds.Advisory) { a.Direction = "arrival" }},
		{"arrival airport", func(a *feeds.Advisory) { a.Airport = "KJFK" }},
		{"different airport in raw text", func(a *feeds.Advisory) { a.RawReason = "!JFK JFK AD AP CLSD" }},
		{"raw runway", func(a *feeds.Advisory) { a.RawReason = "RWY 02 CLSD" }},
		{"raw conditional closure", func(a *feeds.Advisory) { a.RawReason = "AD AP CLSD EXC SKED ACFT" }},
		{"runway closure context", func(a *feeds.Advisory) { a.Kind, a.RawReason = "runway_closure", "RWY 02 CLSD" }},
		{"taxiway closure context", func(a *feeds.Advisory) { a.Kind, a.RawReason = "taxiway_closure", "TWY A CLSD" }},
		{"airport restriction context", func(a *feeds.Advisory) { a.Kind = "airport_restriction" }},
		{"cancellation kind context", func(a *feeds.Advisory) { a.Kind = "cancellation" }},
		{"NOTAM cancellation kind context", func(a *feeds.Advisory) { a.Kind = "notam_cancelled" }},
		{"cancellation", func(a *feeds.Advisory) { a.RawReason = "(A1234/26 NOTAMC A1233/26\nA) KSFO\nE) AD CLSD)" }},
		{"attached estimated end", func(a *feeds.Advisory) { a.RawReason = "AD AP CLSD 2611051900-2611052000EST" }},
		{"estimated ICAO end", func(a *feeds.Advisory) {
			a.RawReason = "(A1234/26 NOTAMN\nA) KSFO B) 2611051900 C) 2611052000 EST\nE) AD CLSD)"
		}},
		{"permanent ICAO end", func(a *feeds.Advisory) { a.RawReason = "(A1234/26 NOTAMN\nA) KSFO B) 2611051900 C) PERM\nE) AD CLSD)" }},
		{"missing end", func(a *feeds.Advisory) { a.EndsAt = nil }},
		{"stale observation", func(a *feeds.Advisory) { a.ObservedAt = now.Add(-6 * time.Hour) }},
		{"future observation", func(a *feeds.Advisory) { a.ObservedAt = now.Add(time.Second) }},
		{"closure ends at departure", func(a *feeds.Advisory) { a.EndsAt = &departure }},
		{"closure begins after departure", func(a *feeds.Advisory) { a.StartsAt = new(departure.Add(time.Second)) }},
		{"closure extends beyond the forecast horizon", func(a *feeds.Advisory) { a.EndsAt = new(departure.Add(25 * time.Hour)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			advisory := valid
			tc.change(&advisory)
			if got := Predict(flight, nil, []feeds.Advisory{advisory}, now); got != nil {
				t.Fatalf("context-only advisory changed the forecast: %+v", got)
			}
		})
	}
}
