package core

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestSWIMRadarPrivateIndependentPaging(t *testing.T) {
	s, flight, report, now := swimRadarFixture(t)
	ctx := t.Context()
	first := report.Positions[0]
	provider := first
	provider.Source = flight.Source
	if err := s.Observe(ctx, flight, []feeds.Position{provider}, nil, nil); err != nil {
		t.Fatal(err)
	}
	owner := trackSWIMRadarFixture(t, s, flight.ID)
	receiver := first
	receiver.Source = "operator_readsb"
	receiver.Latitude++
	if err := storeRadarSamples(ctx, s, []radarSample{{flight, receiver}}); err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []func(*feeds.RadarPositionReport){
		func(r *feeds.RadarPositionReport) { r.ProviderFlightID = "unknown-instance" },
		func(r *feeds.RadarPositionReport) { r.OriginICAO = "KOAK" },
		func(r *feeds.RadarPositionReport) { r.DestinationICAO = "KJFK" },
		func(r *feeds.RadarPositionReport) { r.TailNumber = "N222AA" },
		func(r *feeds.RadarPositionReport) { r.ScheduledDeparture = r.ScheduledDeparture.Add(time.Nanosecond) },
		func(r *feeds.RadarPositionReport) { r.ActualTakeoff = r.ActualTakeoff.Add(time.Nanosecond) },
	} {
		mismatch := report
		wrong(&mismatch)
		recordSWIMRadarFixture(t, s, mismatch, now)
	}
	lateReport := report
	late := first
	late.At = now.Add(16 * time.Minute)
	lateReport.Positions = []feeds.Position{late}
	recordSWIMRadarFixture(t, s, lateReport, late.At)
	if empty := readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil); empty.SWIMRevision == nil || len(empty.SWIMPositions) != 0 {
		t.Fatalf("mismatched or stale reports yielded %+v", empty)
	}
	recordSWIMRadarFixture(t, s, report, now)
	initial := readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil)
	if initial.SWIMRevision == nil || *initial.SWIMRevision != 0 || len(initial.SWIMPositions) != 1 ||
		initial.SWIMPositions[0].Source != feeds.FAASWIM || initial.SWIMPositions[0].Kind != "radar" ||
		len(initial.Positions) != 1 || len(initial.ProviderPositions) != 1 {
		t.Fatalf("sources did not page independently: %+v", initial)
	}
	second := first
	second.At = first.At.Add(time.Nanosecond)
	report.Positions = []feeds.Position{first, second}
	recordSWIMRadarFixture(t, s, report, now)
	older := first
	older.At = older.At.Add(-time.Nanosecond)
	report.Positions = []feeds.Position{older}
	recordSWIMRadarFixture(t, s, report, now)
	next := readSWIMRadarFixture(t, s, owner, flight.ID, &first.At, initial.SWIMRevision)
	if len(next.SWIMPositions) != 1 || !next.SWIMPositions[0].At.Equal(second.At) ||
		len(readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil).SWIMPositions) != 2 {
		t.Fatalf("append or replay handling failed: %+v", next)
	}
	private := readSWIMRadarFixture(t, s, "unrelated", flight.ID, nil, nil)
	if private.SWIMRevision != nil || private.SWIMPositions == nil || len(private.SWIMPositions) != 0 {
		t.Fatalf("untracked reader received SWIM data: %+v", private)
	}
	detail, err := s.DetailsForInstallation(ctx, owner, flight.ID)
	if err != nil || !detail.Flight.ObservedAt.Equal(flight.ObservedAt) || len(detail.Telemetry) != 2 || len(detail.Events) != 0 {
		t.Fatalf("position report changed the flight snapshot: %+v, %v", detail, err)
	}
	_, pickup, err := s.CreatePickupLink(ctx, owner, flight.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, shared, err := s.SharedPickup(ctx, pickup, now)
	if err != nil || len(shared) != 1 || !shared[0].At.Equal(provider.At) {
		t.Fatalf("pickup did not preserve its provider-only track: %+v, %v", shared, err)
	}
}

func TestSWIMRadarRequiresUniqueTrackedProviderBinding(t *testing.T) {
	s, flight, report, now := swimRadarFixture(t)
	ctx := t.Context()
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Exercise legacy tracked exact AeroAPI IDs without an explicit alias.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM aero_instance_keys WHERE flight_id=?`, flight.ID); err != nil {
		t.Fatal(err)
	}
	recordSWIMRadarFixture(t, s, report, now)
	owner := trackSWIMRadarFixture(t, s, flight.ID)
	if empty := readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil); len(empty.SWIMPositions) != 0 {
		t.Fatalf("report was accepted before a flight was tracked: %+v", empty)
	}
	alias := flight
	alias.ID = "local-schedule-alias"
	if err := s.Observe(ctx, alias, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE aero_instance_keys SET provider_id=? WHERE flight_id=?`, flight.ID, alias.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, alias.ID, ""); err != nil {
		t.Fatal(err)
	}
	recordSWIMRadarFixture(t, s, report, now)
	for _, id := range []string{flight.ID, alias.ID} {
		if page := readSWIMRadarFixture(t, s, owner, id, nil, nil); len(page.SWIMPositions) != 0 {
			t.Fatalf("ambiguous provider key yielded fixes for %s: %+v", id, page)
		}
	}
	if err := s.Untrack(ctx, owner, flight.ID); err != nil {
		t.Fatal(err)
	}
	recordSWIMRadarFixture(t, s, report, now)
	if page := readSWIMRadarFixture(t, s, owner, alias.ID, nil, nil); len(page.SWIMPositions) != 1 {
		t.Fatalf("unique tracked alias was not resolved: %+v", page)
	}
}

func TestSWIMRadarScopeResetAndLanding(t *testing.T) {
	s, flight, report, now := swimRadarFixture(t)
	ctx := t.Context()
	observe := func(f feeds.Flight) {
		t.Helper()
		if err := s.Observe(ctx, f, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	observe(flight)
	owner := trackSWIMRadarFixture(t, s, flight.ID)
	recordSWIMRadarFixture(t, s, report, now)
	initial := readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil)
	if len(initial.SWIMPositions) != 1 {
		t.Fatalf("initial page %+v", initial)
	}
	changed := flight
	changed.DestinationICAO, changed.TailNumber, changed.ObservedAt = "KJFK", "N222AA", now.Add(time.Second)
	observe(changed)
	old := report.Positions[0]
	reset := readSWIMRadarFixture(t, s, owner, flight.ID, &old.At, initial.SWIMRevision)
	if reset.SWIMRevision == nil || *reset.SWIMRevision != changed.ObservedAt.UnixNano() || len(reset.SWIMPositions) != 0 {
		t.Fatalf("changed movement retained previous radar fixes: %+v", reset)
	}
	flight.ObservedAt = now.Add(2 * time.Second)
	observe(flight)
	between := old
	between.At = now.Add(1500 * time.Millisecond)
	newFix := old
	newFix.At = now.Add(3 * time.Second)
	report.TailNumber = flight.TailNumber
	report.Positions = []feeds.Position{old, between, newFix}
	recordSWIMRadarFixture(t, s, report, newFix.At)
	// A stale revision takes precedence over a stale cursor, even one ahead of the new path.
	ahead := now.Add(time.Minute)
	replaced := readSWIMRadarFixture(t, s, owner, flight.ID, &ahead, initial.SWIMRevision)
	if replaced.SWIMRevision == nil || *replaced.SWIMRevision != flight.ObservedAt.UnixNano() || len(replaced.SWIMPositions) != 1 ||
		!replaced.SWIMPositions[0].At.Equal(newFix.At) {
		t.Fatalf("scope restoration accepted a previous movement or missed the fresh one: %+v", replaced)
	}
	flight.Status, flight.ObservedAt = "landed", now.Add(4*time.Second)
	flight.ActualLanding = &flight.ObservedAt
	observe(flight)
	newFix.At = now.Add(5 * time.Second)
	report.Positions = []feeds.Position{newFix}
	recordSWIMRadarFixture(t, s, report, newFix.At)
	landed := readSWIMRadarFixture(t, s, owner, flight.ID, nil, replaced.SWIMRevision)
	if landed.SWIMRevision == nil || *landed.SWIMRevision != *replaced.SWIMRevision || len(landed.SWIMPositions) != 1 ||
		!landed.SWIMPositions[0].At.Equal(replaced.SWIMPositions[0].At) {
		t.Fatalf("landing did not preserve the last airborne fix: %+v", landed)
	}
	if err := s.Prune(ctx, now.Add(trafficRadarRetention+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if page := readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil); len(page.SWIMPositions) != 0 {
		t.Fatalf("expired fixes were retained: %+v", page)
	}
}

func TestSWIMRadarDelayedScopeChangeRejectsErasedFixReplay(t *testing.T) {
	s, flight, report, now := swimRadarFixture(t)
	ctx := t.Context()
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	owner := trackSWIMRadarFixture(t, s, flight.ID)
	recordSWIMRadarFixture(t, s, report, now)
	flight.TailNumber = "N222AA"
	flight.ObservedAt = now.Add(-2 * time.Second)
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	recordSWIMRadarFixture(t, s, report, now)
	if page := readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil); page.SWIMRevision == nil || len(page.SWIMPositions) != 0 {
		t.Fatalf("delayed scope change allowed erased fix to reappear: %+v", page)
	}
	// A newer timestamp clears the time floor but does not identify the plane.
	report.Positions[0].At = now
	for _, tail := range []string{"", "N123AA"} {
		report.TailNumber = tail
		recordSWIMRadarFixture(t, s, report, now)
		if page := readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil); len(page.SWIMPositions) != 0 {
			t.Fatalf("fresh fix for tail %q repopulated the previous track: %+v", tail, page)
		}
	}
	report.TailNumber = "n222aa"
	recordSWIMRadarFixture(t, s, report, now)
	if page := readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil); len(page.SWIMPositions) != 1 || !page.SWIMPositions[0].At.Equal(now) {
		t.Fatalf("updated bridge binding did not admit its fresh fix: %+v", page)
	}
}

func TestSWIMRadarRequiresTailWhenTakeoffReturnsWithNewAircraft(t *testing.T) {
	s, flight, report, now := swimRadarFixture(t)
	observe := func() {
		t.Helper()
		if err := s.Observe(t.Context(), flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	observe()
	owner := trackSWIMRadarFixture(t, s, flight.ID)
	recordSWIMRadarFixture(t, s, report, now)
	takeoff := flight.ActualTakeoff
	flight.ActualTakeoff, flight.ObservedAt = nil, now.Add(-3*time.Second)
	observe()
	flight.ActualTakeoff, flight.TailNumber, flight.ObservedAt = takeoff, "N222AA", now.Add(-2*time.Second)
	observe()
	report.Positions[0].At = now
	recordSWIMRadarFixture(t, s, report, now)
	if page := readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil); len(page.SWIMPositions) != 0 {
		t.Fatalf("unbound fix entered a scope after the takeoff was restored: %+v", page)
	}
	report.TailNumber = flight.TailNumber
	recordSWIMRadarFixture(t, s, report, now)
	if page := readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil); len(page.SWIMPositions) != 1 {
		t.Fatalf("current tail did not recover the track: %+v", page)
	}
}

func TestSWIMRadarReadSkipsExpiredFixesBeforePrune(t *testing.T) {
	s, flight, report, now := swimRadarFixture(t)
	ctx := t.Context()
	ago := -(trafficRadarRetention + time.Hour)
	flight.ObservedAt = flight.ObservedAt.Add(ago)
	flight.ScheduledDeparture = new(flight.ScheduledDeparture.Add(ago))
	flight.ActualTakeoff = new(flight.ActualTakeoff.Add(ago))
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	owner := trackSWIMRadarFixture(t, s, flight.ID)
	report.ScheduledDeparture = *flight.ScheduledDeparture
	report.ActualTakeoff = *flight.ActualTakeoff
	report.Positions[0].At = report.Positions[0].At.Add(ago)
	recordSWIMRadarFixture(t, s, report, now.Add(ago))
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM swim_radar_positions WHERE flight_id=?`, flight.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("fixture did not persist the old fix: %d, %v", rows, err)
	}
	if page := readSWIMRadarFixture(t, s, owner, flight.ID, nil, nil); len(page.SWIMPositions) != 0 {
		t.Fatalf("expired fix was displayed before pruning: %+v", page)
	}
}

func swimRadarFixture(t *testing.T) (*Store, feeds.Flight, feeds.RadarPositionReport, time.Time) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "swim-radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	flight := feeds.Flight{
		ID: "UAL123-swim-fixture", Number: "UA123", Source: "flightaware_aeroapi", Status: "airborne", TailNumber: "N123AA",
		Origin: "SFO", Destination: "EWR", OriginICAO: "KSFO", DestinationICAO: "KEWR", ObservedAt: now.Add(-time.Minute),
		ScheduledDeparture: new(now.Add(-time.Hour)), ActualTakeoff: new(now.Add(-50 * time.Minute)),
	}
	report := feeds.RadarPositionReport{
		ProviderFlightID: flight.ID, OriginICAO: flight.OriginICAO, DestinationICAO: flight.DestinationICAO,
		ScheduledDeparture: *flight.ScheduledDeparture, ActualTakeoff: *flight.ActualTakeoff,
		Positions: []feeds.Position{{Latitude: 42, Longitude: -96, At: now.Add(-time.Second)}},
	}
	return s, flight, report, now
}

func trackSWIMRadarFixture(t *testing.T, s *Store, id string) string {
	t.Helper()
	ctx := t.Context()
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, id, ""); err != nil {
		t.Fatal(err)
	}
	return owner
}

func recordSWIMRadarFixture(t *testing.T, s *Store, report feeds.RadarPositionReport, now time.Time) {
	t.Helper()
	if err := s.RecordSWIMPositions(t.Context(), report, now); err != nil {
		t.Fatal(err)
	}
}

func readSWIMRadarFixture(t *testing.T, s *Store, owner, id string, after *time.Time, known *int64) RadarPage {
	t.Helper()
	page, err := s.RadarPageWithTraffic(t.Context(), owner, id, nil, nil, nil, nil, 256, 256, 0, &RadarCursor{After: after, Revision: known}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return page
}
