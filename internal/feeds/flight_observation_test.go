package feeds

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func fixtureFlightObservation(t *testing.T, observedAt time.Time, update func(map[string]any)) []byte {
	t.Helper()
	flight := map[string]any{
		"fa_flight_id": "UAL7-bridge-fixture", "ident": "UAL7", "ident_iata": "UA7", "ident_icao": "UAL7",
		"status": "Landed", "blocked": false, "cancelled": false, "diverted": false,
		"origin":       map[string]string{"code": "KSFO", "code_iata": "SFO", "code_icao": "KSFO", "timezone": "America/Los_Angeles"},
		"destination":  map[string]string{"code": "KJFK", "code_iata": "JFK", "code_icao": "KJFK", "timezone": "America/New_York"},
		"registration": "N12345", "aircraft_type": "B739", "inbound_fa_flight_id": "UAL8-bridge-fixture", "atc_ident": nil, "route_distance": 2586,
		"gate_origin": "C2", "gate_destination": "B8", "terminal_origin": "C", "terminal_destination": nil, "baggage_claim": nil,
		"scheduled_out": observedAt.Add(-5 * time.Hour), "scheduled_in": observedAt.Add(20 * time.Minute), "scheduled_off": nil, "scheduled_on": nil,
		"estimated_out": nil, "estimated_in": nil, "actual_out": observedAt.Add(-5 * time.Hour),
		"actual_off": observedAt.Add(-4 * time.Hour), "actual_on": observedAt.Add(-5 * time.Minute), "actual_in": nil,
		"other_provider_property": "not used by Plano",
	}
	if update != nil {
		update(flight)
	}
	body, err := json.Marshal(map[string]any{"version": 1, "observedAt": observedAt, "flight": flight})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestDecodeFlightObservationNormalizesCompleteSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)
	observed := now.Add(-time.Minute).In(time.FixedZone("fixture", -4*60*60))
	body := fixtureFlightObservation(t, observed, nil)
	flight, err := DecodeFlightObservation(bytes.NewReader(body), now)
	if err != nil {
		t.Fatal(err)
	}
	if flight.ID != "UAL7-bridge-fixture" || flight.AeroProviderID() != flight.ID || flight.Source != aeroSource ||
		flight.Number != "UA7" || flight.ICAONumber != "UAL7" || flight.Origin != "SFO" || flight.OriginICAO != "KSFO" ||
		flight.Destination != "JFK" || flight.DestinationTimeZone != "America/New_York" || flight.InboundID != "UAL8-bridge-fixture" ||
		!flight.ObservedAt.Equal(observed) || flight.ObservedAt.Location() != time.UTC || flight.Status != "landed" ||
		flight.ActualLanding == nil || !flight.ActualLanding.Equal(observed.Add(-5*time.Minute)) || flight.ActualArrival != nil ||
		flight.ArrivalGate != "B8" || flight.ArrivalTerminal != "" || flight.Baggage != "" {
		t.Fatalf("unexpected flight %+v", flight)
	}
	body = fixtureFlightObservation(t, observed, func(f map[string]any) {
		f["scheduled_off"], f["scheduled_out"] = f["scheduled_out"], nil
	})
	if flight, err := DecodeFlightObservation(bytes.NewReader(body), now); err != nil || flight.ScheduledDeparture == nil || !flight.ScheduledDeparture.Equal(observed.Add(-5*time.Hour)) {
		t.Fatalf("runway-only schedule returned %+v, %v", flight, err)
	}
	body = fixtureFlightObservation(t, observed, func(f map[string]any) {
		f["actual_off"], f["actual_in"] = nil, f["actual_on"]
	})
	if flight, err := DecodeFlightObservation(bytes.NewReader(body), now); err != nil || flight.ActualTakeoff != nil || flight.ActualArrival == nil || flight.ActualLanding == nil || !flight.ActualArrival.Equal(*flight.ActualLanding) {
		t.Fatalf("missing and equal milestones returned %+v, %v", flight, err)
	}
}

func TestDecodeFlightObservationWithholdsBlockedAndUsesCancellationStatus(t *testing.T) {
	now := time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)
	for _, unavailable := range []func(map[string]any){
		func(f map[string]any) { f["blocked"] = true },
		func(f map[string]any) { f["error"] = "provider could not assemble details" },
	} {
		body := fixtureFlightObservation(t, now, unavailable)
		if flight, err := DecodeFlightObservation(bytes.NewReader(body), now); !errors.Is(err, ErrNotFound) || flight.ID != "" {
			t.Fatalf("unavailable snapshot returned %+v, %v", flight, err)
		}
	}
	for label, status := range map[string]string{"Scheduled": "unknown", "Cancelled": "cancelled"} {
		body := fixtureFlightObservation(t, now, func(f map[string]any) {
			f["cancelled"], f["status"] = true, label
			f["actual_out"], f["actual_off"], f["actual_on"] = nil, nil, nil
		})
		if flight, err := DecodeFlightObservation(bytes.NewReader(body), now); err != nil || flight.Status != status {
			t.Fatalf("snapshot with label %q returned %+v, %v, want status %q", label, flight, err, status)
		}
	}
}

func TestDecodeFlightObservationRejectsInvalidAndPartialSnapshots(t *testing.T) {
	now := time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)
	valid := string(fixtureFlightObservation(t, now, nil))
	invalid := map[string]string{
		"callback":        "{\"alert_id\":1,\"event_code\":\"change\",\"flight\":{\"fa_flight_id\":\"UAL7-bridge-fixture\"}}",
		"version":         strings.Replace(valid, "\"version\":1", "\"version\":2", 1),
		"oversized":       valid + strings.Repeat(" ", MaxFlightObservationBytes),
		"trailing object": valid + "{}",
		"invalid utf8":    strings.Replace(valid, "B8", string([]byte{0xff}), 1),
		"stale capture":   string(fixtureFlightObservation(t, now.Add(-5*time.Minute-time.Nanosecond), nil)),
		"future capture":  string(fixtureFlightObservation(t, now.Add(time.Nanosecond), nil)),
	}
	for name, update := range map[string]func(map[string]any){
		"missing nullable field":  func(f map[string]any) { delete(f, "baggage_claim") },
		"missing status":          func(f map[string]any) { delete(f, "status") },
		"missing privacy flag":    func(f map[string]any) { delete(f, "blocked") },
		"null privacy flag":       func(f map[string]any) { f["blocked"] = nil },
		"provider traversal":      func(f map[string]any) { f["fa_flight_id"] = "../fixture" },
		"inbound circular":        func(f map[string]any) { f["inbound_fa_flight_id"] = f["fa_flight_id"] },
		"inbound traversal":       func(f map[string]any) { f["inbound_fa_flight_id"] = "../fixture" },
		"invalid designator":      func(f map[string]any) { f["ident_iata"] = "Invalid" },
		"invalid ICAO designator": func(f map[string]any) { f["ident_icao"] = "Invalid" },
		"invalid airport code":    func(f map[string]any) { f["destination"] = map[string]string{"code_iata": "J&K"} },
		"callback airport shape":  func(f map[string]any) { f["destination"] = "KJFK" },
		"missing departure":       func(f map[string]any) { f["scheduled_out"] = nil },
		"historical departure":    func(f map[string]any) { f["scheduled_out"] = now.Add(-36*time.Hour - time.Second) },
		"distant departure":       func(f map[string]any) { f["scheduled_out"] = now.Add(48*time.Hour + time.Second) },
		"future actual":           func(f map[string]any) { f["actual_in"] = now.Add(time.Second) },
		"reversed actuals":        func(f map[string]any) { f["actual_out"] = now.Add(-3 * time.Hour) },
		"malformed arrival":       func(f map[string]any) { f["estimated_in"] = "2026-11-06T21:00:00" },
		"control in gate":         func(f map[string]any) { f["gate_origin"] = "C12\nD12" },
		"oversized status":        func(f map[string]any) { f["status"] = strings.Repeat("A", 129) },
	} {
		invalid[name] = string(fixtureFlightObservation(t, now, update))
	}
	for name, body := range invalid {
		t.Run(name, func(t *testing.T) {
			if flight, err := DecodeFlightObservation(strings.NewReader(body), now); !errors.Is(err, ErrInvalidQuery) || flight.ID != "" {
				t.Fatalf("invalid snapshot returned %+v, %v", flight, err)
			}
		})
	}
}
