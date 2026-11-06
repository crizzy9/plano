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

func TestPredictionUsesVerifiedPredepartureObservationAsFloor(t *testing.T) {
	scheduled := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	observed, now := scheduled.Add(42*time.Minute+30*time.Second), scheduled.Add(55*time.Minute)
	published := scheduled.Add(10 * time.Minute)
	flight := feeds.Flight{ID: "late-departure", Status: "boarding", Origin: "SFO", OriginICAO: "KSFO",
		ScheduledDeparture: &scheduled, EstimatedDeparture: &published, ObservedAt: observed}
	context := feeds.Advisory{Airport: "KSFO", Kind: feeds.AdvisoryAirportNotice, Source: feeds.FAASWIM,
		Reason: "weather and ATC congestion in contextual notice", ObservedAt: now, DelayMinutes: new(90)}
	if got := Predict(flight, nil, []feeds.Advisory{context}, now); got != nil {
		t.Fatalf("elapsed time and airport context alone generated a forecast: %+v", got)
	}
	delay := feeds.Advisory{Airport: "KSFO", Direction: "departure", Kind: "departure_delay", Source: feeds.FAASWIM,
		Reason: "airport volume", ObservedAt: now, DelayMinutes: new(20)}
	got := Predict(flight, nil, []feeds.Advisory{context, delay}, now)
	if got == nil || !got.Departure.Equal(observed) || got.DelayMinutes != 43 || len(got.Reasons) != 2 ||
		!strings.Contains(got.Reasons[1], "last flight update") || len(got.AdvisoryEvidence) != 1 {
		t.Fatalf("elapsed departure floor = %+v", got)
	}
	legacy := *got
	legacy.Departure = &published
	if _, valid := predictionValidity(&legacy, flight, nil, now); valid {
		t.Fatal("saved forecast survived a later predeparture observation")
	}
	features, ok := ExtractDelayFeatures(flight, nil, []feeds.Advisory{context, delay}, now)
	if !ok || features != (predict.Features{AirlineDelayMinutes: 10, AirportDelayMinutes: 20, AirTrafficControl: true}) ||
		!flight.EstimatedDeparture.Equal(published) || !flight.ScheduledDeparture.Equal(scheduled) {
		t.Fatalf("forecast changed published times or features: %+v, %t", features, ok)
	}
	flight.ObservedAt = now.Add(time.Second)
	if got := Predict(flight, nil, []feeds.Advisory{delay}, now); got == nil || !got.Departure.Equal(now) {
		t.Fatalf("forecast used an observation beyond the decision time: %+v", got)
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
	closureStart, closureEnd := row.ScheduledDeparture.Add(45*time.Minute), row.ScheduledDeparture.Add(100*time.Minute)
	daily := fmt.Sprintf("AD AP CLSD DLY %s-%s %s-%s", closureStart.Format("1504"), closureEnd.Format("1504"),
		closureStart.Format("0601021504"), closureEnd.Format("0601021504"))
	for _, raw := range []string{"AD AP CLSD", daily} {
		closure := feeds.Advisory{Airport: "SFO", Direction: "departure", Kind: "airport_closure", Source: "faa_nas", RawReason: raw,
			ObservedAt: row.ObservedAt, StartsAt: &closureStart, EndsAt: &closureEnd}
		closed := PredictWithModel(model, flight, nil, []feeds.Advisory{closure}, row.ObservedAt)
		if closed == nil || closed.Method != got.Method || closed.ModelVersion != got.ModelVersion || closed.DelayMinutes != 100 ||
			!closed.Departure.Equal(closureEnd) || !slices.Equal(closed.FeatureContributions, got.FeatureContributions) ||
			closed.ValidUntil == nil || !closed.ValidUntil.Equal(row.ObservedAt.Add(30*time.Minute)) ||
			len(closed.Reasons) == 0 || !strings.Contains(closed.Reasons[len(closed.Reasons)-1], "full airport closure") {
			t.Fatalf("closure-adjusted learned forecast for %q = %+v", raw, closed)
		}
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

func TestPredictionUsesOnlyTheMatchingDailyClosure(t *testing.T) {
	at := func(hour, minute int) time.Time {
		return time.Date(2026, time.November, 6, hour, minute, 0, 0, time.UTC)
	}
	const daytime = "!SFO SFO AD AP CLSD DLY 0730-0815 2611050730-2611080815"
	const overnight = "(A1234/26 NOTAMN\nA) KSFO B) 2611052330 C) 2611070100\nD) DLY 2330-0100\nE) AD CLSD)"
	const clipped = "!SFO SFO AD AP CLSD DLY 0730-0815 2611050730-2611060805"
	for _, tc := range []struct {
		name, raw           string
		now, departure, end time.Time
	}{
		{"before today's closure", daytime, at(7, 25), at(7, 29), time.Time{}},
		{"at the start", daytime, at(7, 25), at(7, 30), at(8, 15)},
		{"during today's closure", daytime, at(7, 55), at(8, 0), at(8, 15)},
		{"at the end", daytime, at(7, 55), at(8, 15), time.Time{}},
		{"between occurrences", daytime, at(8, 0), at(15, 0), time.Time{}},
		{"in yesterday's overnight closure", overnight, at(0, 45), at(0, 50), at(1, 0)},
		{"at the end of yesterday's overnight closure", overnight, at(0, 45), at(1, 0), time.Time{}},
		{"clipped by the notice end", clipped, at(7, 55), at(8, 0), at(8, 5)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flight := feeds.Flight{Status: "scheduled", Origin: "SFO", OriginICAO: "KSFO", ScheduledDeparture: &tc.departure}
			closure := feeds.Advisory{ID: "daily", Airport: "KSFO", Kind: "airport_closure", Source: feeds.FAASWIM,
				RawReason: tc.raw, ObservedAt: tc.now}
			got := Predict(flight, nil, []feeds.Advisory{closure}, tc.now)
			if tc.end.IsZero() {
				if got != nil {
					t.Fatalf("flight outside the occurrence got closure forecast %+v", got)
				}
				return
			}
			if got == nil || got.Departure == nil || !got.Departure.Equal(tc.end) || got.DelayMinutes != int(tc.end.Sub(tc.departure).Minutes()) ||
				got.ValidUntil == nil || !got.ValidUntil.Equal(earliestTime(tc.now.Add(advisoryForecastTTL), tc.end)) ||
				!slices.Equal(got.AdvisoryEvidence, []forecastAdvisory{advisoryEvidence(closure)}) ||
				len(got.Reasons) != 1 || !strings.Contains(got.Reasons[0], "full airport closure at KSFO") {
				t.Fatalf("forecast = %+v, want closure until %s", got, tc.end)
			}
		})
	}
}

func TestPredictionChainsDailyAndFixedClosures(t *testing.T) {
	now := time.Date(2026, time.November, 6, 7, 30, 0, 0, time.UTC)
	departure := now.Add(5 * time.Minute)
	flight := feeds.Flight{Status: "scheduled", Origin: "SFO", OriginICAO: "KSFO", ScheduledDeparture: &departure}
	before := feeds.Advisory{ID: "before", Airport: "SFO", Kind: "airport_closure", Source: "faa_nas", RawReason: "AD AP CLSD",
		ObservedAt: now, EndsAt: new(now.Add(10 * time.Minute))}
	daily := feeds.Advisory{ID: "daily", Airport: "KSFO", Kind: "airport_closure", Source: feeds.FAASWIM, ObservedAt: now,
		RawReason: "!SFO SFO AD AP CLSD DLY 0736-0750 2611050736-2611080750"}
	after := feeds.Advisory{ID: "after", Airport: "SFO", Kind: "airport_closure", Source: "faa_nas", RawReason: "AD AP CLSD",
		ObservedAt: now, StartsAt: new(now.Add(15 * time.Minute)), EndsAt: new(now.Add(55 * time.Minute))}
	wantEvidence := []forecastAdvisory{advisoryEvidence(before), advisoryEvidence(daily), advisoryEvidence(after)}
	for _, rows := range [][]feeds.Advisory{{before, daily, after}, {after, daily, before}} {
		got := Predict(flight, nil, rows, now)
		if got == nil || got.Departure == nil || !got.Departure.Equal(*after.EndsAt) ||
			got.ValidUntil == nil || !got.ValidUntil.Equal(*before.EndsAt) || !slices.Equal(got.AdvisoryEvidence, wantEvidence) {
			t.Fatalf("forecast across fixed and daily closures = %+v", got)
		}
	}
}

func TestPredictionChainsClosureAndGroundStop(t *testing.T) {
	now := time.Date(2026, time.November, 5, 18, 0, 0, 0, time.UTC)
	scheduled := now.Add(20 * time.Minute)
	flight := feeds.Flight{Status: "scheduled", Origin: "SFO", Destination: "JFK", ScheduledDeparture: &scheduled}
	closure := feeds.Advisory{ID: "closure", Airport: "SFO", Kind: "airport_closure", Direction: "departure", Source: "faa_nas",
		RawReason: "AD AP CLSD", ObservedAt: now, StartsAt: new(now.Add(15 * time.Minute)), EndsAt: new(now.Add(50 * time.Minute))}
	stop := feeds.Advisory{ID: "stop", Airport: "JFK", Kind: "ground_stop", Direction: "arrival", Source: feeds.FAASWIM,
		ObservedAt: now, StartsAt: new(now.Add(40 * time.Minute)), EndsAt: new(now.Add(70 * time.Minute))}
	context := stop
	context.ID, context.AirportContextOnly = "context", true
	context.StartsAt, context.EndsAt = new(now.Add(time.Hour)), new(now.Add(90*time.Minute))
	advisories := []feeds.Advisory{context, stop, closure}
	got := Predict(flight, nil, advisories, now)
	if got == nil || got.Departure == nil || !got.Departure.Equal(*stop.EndsAt) || got.DelayMinutes != 50 ||
		!slices.Equal(got.AdvisoryEvidence, []forecastAdvisory{advisoryEvidence(closure), advisoryEvidence(stop)}) ||
		len(got.Reasons) != 2 || !strings.Contains(got.Reasons[0], "full airport closure at SFO") || !strings.Contains(got.Reasons[1], "ground stop at JFK") {
		t.Fatalf("forecast should follow both applicable holds and skip airport-only context: %+v", got)
	}
	if features, ok := ExtractDelayFeatures(flight, nil, advisories, now); !ok || features != (predict.Features{}) {
		t.Fatalf("post-estimate holds changed the model features: %+v, %t", features, ok)
	}
}

func TestPredictionChecksDailyClosureAtTheForecastLimit(t *testing.T) {
	now := time.Date(2026, time.November, 6, 8, 0, 0, 0, time.UTC)
	flight := feeds.Flight{Status: "scheduled", Origin: "SFO", OriginICAO: "KSFO", ScheduledDeparture: &now}
	limit := now.Add(departureForecastLimit)
	before := feeds.Advisory{ID: "before", Airport: "SFO", Kind: "airport_closure", Source: "faa_nas", RawReason: "AD AP CLSD",
		ObservedAt: now, EndsAt: &limit}
	daily := feeds.Advisory{ID: "daily", Airport: "KSFO", Kind: "airport_closure", Source: feeds.FAASWIM, ObservedAt: now,
		RawReason: "!SFO SFO AD AP CLSD DLY 0800-0900 2611070800-2611080900"}
	if got := Predict(flight, nil, []feeds.Advisory{before}, now); got == nil || got.Departure == nil || !got.Departure.Equal(limit) {
		t.Fatalf("forecast at the delay limit = %+v", got)
	}
	if got := Predict(flight, nil, []feeds.Advisory{daily, before}, now); got != nil {
		t.Fatalf("daily closure begins exactly at the limit but forecast continues: %+v", got)
	}
	flight.EstimatedDeparture = new(limit.Add(30 * time.Minute))
	delay := feeds.Advisory{ID: "wind", Airport: "KSFO", Kind: "departure_delay", Direction: "departure", Source: "faa_nas",
		ObservedAt: now, Reason: "wind"}
	if got := Predict(flight, nil, []feeds.Advisory{delay}, now); got == nil || got.Departure == nil || !got.Departure.Equal(*flight.EstimatedDeparture) {
		t.Fatalf("baseline beyond the closure limit = %+v", got)
	}
	if got := Predict(flight, nil, []feeds.Advisory{delay, daily}, now); got != nil {
		t.Fatalf("initial estimate beyond the limit falls in a daily closure: %+v", got)
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
