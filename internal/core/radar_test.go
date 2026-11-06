package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestRadarAppendCursorAndCurrentAircraftGuard(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	dep, arr := now.Add(-time.Hour), now.Add(time.Hour)
	flight := feeds.Flight{ID: "radar-fixture", Number: "XY123", Origin: "LAX", Destination: "EWR", TailNumber: "N123AA", ICAONumber: "XY123", Status: "airborne", ActualTakeoff: &dep, ScheduledDeparture: &dep, ScheduledArrival: &arr, ObservedAt: now}
	provider := feeds.Position{At: now.Add(-time.Minute), Latitude: 33, Longitude: -100, Source: "flightaware_aeroapi"}
	if err := s.Observe(ctx, flight, []feeds.Position{provider}, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	first := feeds.Position{At: now.Add(time.Nanosecond), Latitude: 35, Longitude: -92, Source: "operator_readsb"}
	second := feeds.Position{At: now.Add(2 * time.Nanosecond), Latitude: 35.01, Longitude: -91.99, Source: "operator_readsb"}
	if err := storeRadarSamples(ctx, s, []radarSample{{flight, first}, {flight, first}, {flight, second}}); err != nil {
		t.Fatal(err)
	}
	details, err := s.DetailsForInstallation(ctx, owner, flight.ID)
	if err != nil || !details.Flight.ObservedAt.Equal(now) || len(details.Telemetry) != 3 || len(details.Events) != 0 {
		t.Fatalf("details %#v: %v", details, err)
	}
	foreign, err := s.DetailsForInstallation(ctx, "unrelated", flight.ID)
	if err != nil || len(foreign.Telemetry) != 1 || foreign.Telemetry[0].Source != "flightaware_aeroapi" {
		t.Fatalf("provider/local isolation failed %#v %v", foreign.Telemetry, err)
	}
	foreignRadar, err := s.RadarPositions(ctx, "unrelated", flight.ID, nil, 256)
	if err != nil || len(foreignRadar) != 0 {
		t.Fatalf("foreign live radar %#v %v", foreignRadar, err)
	}
	positions, err := s.RadarPositions(ctx, owner, flight.ID, &first.At, 256)
	if err != nil || len(positions) != 1 || !positions[0].At.Equal(second.At) {
		t.Fatalf("cursor %#v: %v", positions, err)
	}
	newAircraft := flight
	newAircraft.TailNumber, newAircraft.ObservedAt = "N923BB", now.Add(time.Second)
	if err := s.Observe(ctx, newAircraft, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	wrong := feeds.Position{At: now.Add(3 * time.Second), Latitude: 37, Longitude: -89, Source: "operator_readsb"}
	if err := storeRadarSamples(ctx, s, []radarSample{{flight, wrong}}); err != nil {
		t.Fatal(err)
	}
	positions, err = s.RadarPositions(ctx, owner, flight.ID, nil, 256)
	if err != nil || len(positions) != 0 {
		t.Fatalf("wrong tail merged: %#v %v", positions, err)
	}
}

func TestRadarPageResetsAcrossMissingTailAndAircraftChange(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "radar-revision.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	takeoff := now.Add(-time.Hour)
	flight := feeds.Flight{ID: "radar-revision", Number: "XY123", TailNumber: "N1AA", Status: "airborne", ActualTakeoff: &takeoff, ObservedAt: now}
	provider := feeds.Position{At: now.Add(-time.Minute), Latitude: 33, Longitude: -100, Source: "flightaware_aeroapi"}
	old := feeds.Position{At: now.Add(-time.Second), Latitude: 35, Longitude: -92, Source: "operator_readsb"}
	fresh := feeds.Position{At: now.Add(-2 * time.Second), Latitude: 36, Longitude: -91, Source: "operator_readsb"}
	if err := s.Observe(ctx, flight, []feeds.Position{provider, old}, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	sample := func(current feeds.Flight, position feeds.Position) {
		t.Helper()
		if err := storeRadarSamples(ctx, s, []radarSample{{current, position}}); err != nil {
			t.Fatal(err)
		}
	}
	page := func(account string, after *time.Time, known *int64) RadarPage {
		t.Helper()
		result, err := s.RadarPage(ctx, account, flight.ID, after, known, nil, nil, 256, 256)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	observeTail := func(tail string) {
		t.Helper()
		flight.TailNumber = tail
		flight.ObservedAt = flight.ObservedAt.Add(time.Second)
		if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	sample(flight, old)
	initial := page(owner, nil, nil)
	if initial.ReceiverRevision == nil || *initial.ReceiverRevision != 0 || len(initial.Positions) != 1 || len(initial.ProviderPositions) != 1 {
		t.Fatalf("initial receiver page = %+v", initial)
	}
	observeTail("N1AA")
	if result := page(owner, &old.At, initial.ReceiverRevision); result.ReceiverRevision == nil || *result.ReceiverRevision != 0 || len(result.Positions) != 0 {
		t.Fatalf("ordinary observation reset the receiver cursor: %+v", result)
	}
	previous := flight
	observeTail("")
	withdrawn := page(owner, &old.At, initial.ReceiverRevision)
	if withdrawn.ReceiverRevision == nil || *withdrawn.ReceiverRevision == 0 || len(withdrawn.Positions) != 0 || len(withdrawn.ProviderPositions) != 1 {
		t.Fatalf("tail withdrawal should reset receiver only: %+v", withdrawn)
	}
	details, err := s.DetailsForInstallation(ctx, owner, flight.ID)
	if err != nil || len(details.Telemetry) != 1 || details.Telemetry[0].Source != provider.Source {
		t.Fatalf("legacy receiver fix survived tail withdrawal: %+v, %v", details.Telemetry, err)
	}
	observeTail("N1AA")
	if restored := page(owner, &old.At, withdrawn.ReceiverRevision); restored.ReceiverRevision == nil ||
		*restored.ReceiverRevision != *withdrawn.ReceiverRevision || len(restored.Positions) != 0 || len(restored.ProviderPositions) != 1 {
		t.Fatalf("restoring the same tail should preserve provider history: %+v", restored)
	}
	observeTail("")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	observeTail("N2BB")
	sample(previous, old)
	sample(flight, fresh)
	replacement := page(owner, &old.At, initial.ReceiverRevision)
	if replacement.ReceiverRevision == nil || *replacement.ReceiverRevision == *withdrawn.ReceiverRevision ||
		len(replacement.Positions) != 1 || !replacement.Positions[0].At.Equal(fresh.At) || len(replacement.ProviderPositions) != 0 {
		t.Fatalf("new aircraft should replace both cached paths: %+v", replacement)
	}
	staleDetails, err := readRadarPositions(ctx, s.db, owner, flight.ID, nil, new(previous.ObservedAt.UnixNano()), 256)
	if err != nil || len(staleDetails) != 0 {
		t.Fatalf("new receiver fix leaked into older detail snapshot: %+v, %v", staleDetails, err)
	}
	foreign := page("untracked-installation", nil, nil)
	if foreign.ReceiverRevision != nil || foreign.ProviderRevision != nil || foreign.Positions == nil || foreign.ProviderPositions == nil ||
		len(foreign.Positions) != 0 || len(foreign.ProviderPositions) != 0 {
		t.Fatalf("untracked reader got receiver or provider data: %+v", foreign)
	}
}

func TestRadarFirstKnownTailClearsUnattributedLegacyTrack(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "radar-legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	now := time.Now().UTC()
	flight := feeds.Flight{ID: "legacy-blank-tail", Number: "XY123", Status: "airborne", ObservedAt: now}
	provider := feeds.Position{At: now.Add(-time.Minute), Latitude: 33, Longitude: -100, Source: "flightaware_aeroapi"}
	if err := s.Observe(ctx, flight, []feeds.Position{provider}, nil, nil); err != nil {
		t.Fatal(err)
	}
	flight.TailNumber = "N2BB"
	flight.ObservedAt = now.Add(time.Second)
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Details(ctx, flight.ID)
	if err != nil || len(detail.Telemetry) != 0 || detail.Flight.TailNumber != flight.TailNumber {
		t.Fatalf("newly identified aircraft inherited old telemetry: %+v, %v", detail, err)
	}
}

type radarFixtureTransport func(*http.Request) (*http.Response, error)

func (f radarFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func storeRadarSamples(ctx context.Context, s *Store, samples []radarSample) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recordRadarSamples(ctx, tx, samples); err != nil {
		return err
	}
	return tx.Commit()
}

func TestRadarCycleSamplesOnlyTrackedMatchingReceiverFlight(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "radar-cycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	dep, arr := now.Add(-time.Hour), now.Add(time.Hour)
	tracked := feeds.Flight{ID: "sample-tracked", Number: "XY123", Origin: "LAX", Destination: "EWR", TailNumber: "N123AA", ICAONumber: "XY123", Status: "airborne", ActualTakeoff: &dep, ScheduledDeparture: &dep, ScheduledArrival: &arr, ObservedAt: now}
	untracked := tracked
	untracked.ID = "sample-untracked"
	for _, f := range []feeds.Flight{tracked, untracked} {
		if err := s.Observe(ctx, f, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, tracked.ID, ""); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: radarFixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/aircraft.json" {
			t.Fatalf("receiver request %s", r.URL.Path)
		}
		raw := fmt.Sprintf(`{"now":%d,"aircraft":[{"hex":"ab1234","r":"N123AA","flight":"XY123","type":"adsb_icao","lat":42,"lon":-72,"alt_baro":26000,"gs":410,"seen_pos":1}]}`, now.Unix())
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(raw))}, nil
	})}
	receiver, err := feeds.NewADSBReceiver("https://receiver.example.test/aircraft.json", client)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.sampleRadar(ctx, receiver, nil); err != nil {
		t.Fatal(err)
	}
	positions, err := s.RadarPositions(ctx, owner, tracked.ID, nil, 256)
	if err != nil || calls != 1 || len(positions) != 1 || positions[0].Latitude != 42 || positions[0].AltitudeFeet == nil || *positions[0].AltitudeFeet != 26000 {
		t.Fatalf("receiver fixture %#v calls=%d %v", positions, calls, err)
	}
	positions, err = s.RadarPositions(ctx, owner, untracked.ID, nil, 256)
	if err != nil || len(positions) != 0 {
		t.Fatalf("untracked points %#v %v", positions, err)
	}
}

func TestRadarCycleSuppressesOverlappingLegs(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "radar-overlap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	departure, arrival := now.Add(-time.Hour), now.Add(time.Hour)
	first := feeds.Flight{ID: "first", Number: "UA7", ICAONumber: "UAL7", TailNumber: "N123AA", Status: "airborne", ActualTakeoff: &departure, ScheduledArrival: &arrival, ObservedAt: now}
	second := first
	second.ID, second.Number, second.ICAONumber = "second", "UA8", "UAL8"
	other := first
	other.ID, other.TailNumber = "other", "N124AA"
	stale := first
	stale.ID, stale.ObservedAt = "stale", now.Add(-16*time.Minute)
	for _, flight := range []feeds.Flight{first, second, other, stale} {
		if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	track := func(flight feeds.Flight) {
		t.Helper()
		if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	callsign := ""
	client := &http.Client{Transport: radarFixtureTransport(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			// The second leg becomes eligible after the preflight check.
			track(second)
		}
		raw := fmt.Sprintf(`{"now":%d,"aircraft":[
{"hex":"AB1234","r":"N123AA","flight":%q,"type":"adsb_icao","lat":42,"lon":-72,"seen_pos":%d},
{"hex":"ab1235","r":"N124AA","type":"adsb_icao","lat":42,"lon":-72,"seen_pos":%d}]}`,
			now.Unix(), callsign, 4-calls, 4-calls)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(raw))}, nil
	})}
	receiver, err := feeds.NewADSBReceiver("https://receiver.example.test/aircraft.json", client)
	if err != nil {
		t.Fatal(err)
	}
	sample := func() {
		t.Helper()
		if err := s.sampleRadar(ctx, receiver, nil); err != nil {
			t.Fatal(err)
		}
	}
	assertPoints := func(flight feeds.Flight, count int) {
		t.Helper()
		positions, err := s.RadarPositions(ctx, owner, flight.ID, nil, 256)
		if err != nil || len(positions) != count {
			t.Fatalf("%s: got %d points, want %d, err=%v", flight.ID, len(positions), count, err)
		}
	}
	track(stale)
	sample()
	if calls != 0 {
		t.Fatalf("receiver polled without a fresh tracked flight: %d calls", calls)
	}
	track(first)
	track(other)
	sample()
	assertPoints(first, 0)
	assertPoints(second, 0)
	assertPoints(other, 1)

	callsign = "UAL7"
	sample()
	assertPoints(first, 1)
	assertPoints(second, 0)
	assertPoints(other, 2)

	callsign = ""
	second.ActualTakeoff, second.ObservedAt = &now, now.Add(time.Second)
	if err := s.Observe(ctx, second, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	sample()
	assertPoints(first, 2)
	assertPoints(second, 0)
	assertPoints(other, 3)
	assertPoints(stale, 0)
}

func TestRadarSharesOneVerifiedMovementWithItsOnwardRotation(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "rotation-radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Add(-time.Second).Truncate(time.Second)
	takeoff, arrival, onward := now.Add(-time.Hour), now.Add(20*time.Minute), now.Add(time.Hour)
	inbound := feeds.Flight{ID: "inbound", Number: "UA7", Origin: "SFO", Destination: "EWR", TailNumber: "N123AA",
		Status: "airborne", ActualTakeoff: &takeoff, ScheduledArrival: &arrival, ObservedAt: now, Source: "flightaware_aeroapi"}
	parent := feeds.Flight{ID: "onward", Number: "UA8", Origin: "EWR", Destination: "BOS", TailNumber: "N123AA",
		Status: "scheduled", ScheduledDeparture: &onward, InboundID: inbound.ID, ObservedAt: now}
	if err := s.Observe(ctx, parent, nil, []feeds.Flight{inbound}, nil); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	track := func(flight feeds.Flight) {
		t.Helper()
		if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	track(parent)
	calls := 0
	client := &http.Client{Transport: radarFixtureTransport(func(*http.Request) (*http.Response, error) {
		calls++
		raw := fmt.Sprintf(`{"now":%d,"aircraft":[{"hex":"ab1234","r":"N123AA","type":"adsb_icao","lat":42,"lon":-72,"seen_pos":%d}]}`,
			now.Unix(), 8-calls)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(raw))}, nil
	})}
	receiver, err := feeds.NewADSBReceiver("https://receiver.example.test/aircraft.json", client)
	if err != nil {
		t.Fatal(err)
	}
	sample := func() {
		t.Helper()
		if err := s.sampleRadar(ctx, receiver, nil); err != nil {
			t.Fatal(err)
		}
	}
	assertRotationPoints := func(count int) {
		t.Helper()
		_, _, positions, err := s.TrackedRotationRadar(ctx, owner, parent.ID, inbound.ID, time.Now())
		if err != nil || len(positions) != count {
			t.Fatalf("rotation got %d points, want %d, err=%v", len(positions), count, err)
		}
	}
	sample()
	assertRotationPoints(1)
	alias := inbound
	alias.ID = "scheduled_inbound"
	if err := s.Observe(ctx, alias, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO aero_instance_keys(provider_id,flight_id) VALUES(?,?)`, inbound.ID, alias.ID); err != nil {
		t.Fatal(err)
	}
	track(alias)
	sample()
	assertRotationPoints(2)
	conflict := inbound
	conflict.ID, conflict.Number = "different-leg", "UA9"
	if err := s.Observe(ctx, conflict, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	track(conflict)
	sample()
	assertRotationPoints(2)
	positions, err := s.RadarPositions(ctx, owner, alias.ID, nil, 256)
	if err != nil || len(positions) != 1 || calls != 3 {
		t.Fatalf("direct radar got %d points, receiver called %d times, err=%v", len(positions), calls, err)
	}
	if err := s.Untrack(ctx, owner, conflict.ID); err != nil {
		t.Fatal(err)
	}
	parent.InboundID = "newly-assigned"
	parent.ObservedAt = now.Add(time.Second)
	if err := s.Observe(ctx, parent, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, positions, err := s.TrackedRotationRadar(ctx, owner, parent.ID, inbound.ID, time.Now()); !errors.Is(err, ErrNotFound) || len(positions) != 0 {
		t.Fatalf("reassigned leg remained readable, positions=%v, err=%v", positions, err)
	}
	parent.ObservedAt = parent.ObservedAt.Add(time.Nanosecond)
	if err := s.Observe(ctx, parent, nil, []feeds.Flight{inbound}, nil); err != nil {
		t.Fatal(err)
	}
	sample()
	assertRotationPoints(0)

	// A newly verified same-tail fallback is usable after a partial refresh.
	parent.ObservedAt = parent.ObservedAt.Add(time.Nanosecond)
	inbound.ObservedAt = parent.ObservedAt
	if err := s.Observe(ctx, parent, nil, []feeds.Flight{inbound}, nil); err != nil {
		t.Fatal(err)
	}
	sample()
	assertRotationPoints(1)
}
