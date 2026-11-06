package api

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestRadarSnapshotSamplesAllRetainedStreams(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "radar.db")
	store, err := core.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := Server{Store: store, Logger: slog.New(slog.DiscardHandler),
		SWIMPositionKey: make([]byte, 32), EurocontrolPositionKey: make([]byte, 32)}
	token, owner := newImportTestInstallation(t, store)
	otherToken, _ := newImportTestInstallation(t, store)
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-time.Minute)
	flight := feeds.Flight{ID: "radar-combined-snapshot", Number: "XY123", Source: "flightaware_aeroapi",
		OriginICAO: "KSFO", DestinationICAO: "KEWR", TailNumber: "N1AA", Status: "airborne",
		ScheduledDeparture: new(now.Add(-time.Hour)), ActualTakeoff: new(now.Add(-50 * time.Minute)), ObservedAt: now}
	type stream struct {
		source, positions, cursor, after, revision string
		fixes                                      []feeds.Position
	}
	streams := []stream{
		{source: "operator_readsb", positions: "positions", cursor: "cursor", after: "after", revision: "receiverRevision"},
		{source: "flightaware_aeroapi", positions: "providerPositions", cursor: "providerCursor", after: "providerAfter", revision: "providerRevision"},
		{source: feeds.FAASWIM, positions: "swimPositions", cursor: "swimCursor", after: "swimAfter", revision: "swimRevision"},
		{source: feeds.Eurocontrol, positions: "eurocontrolPositions", cursor: "eurocontrolCursor", after: "eurocontrolAfter", revision: "eurocontrolRevision"},
	}
	const fixCount = 520
	for i := range streams {
		for j := range fixCount {
			at := base.Add(time.Duration(j)*time.Millisecond + time.Duration(i)*time.Microsecond)
			streams[i].fixes = append(streams[i].fixes, feeds.Position{At: at, Latitude: 32, Longitude: -95, Source: streams[i].source})
		}
	}
	if err := store.Observe(ctx, flight, streams[1].fixes, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	seedRadarReceiverFixes(t, dbPath, flight.ID, streams[0].fixes)
	report := feeds.RadarPositionReport{ProviderFlightID: flight.ID, OriginICAO: flight.OriginICAO,
		DestinationICAO: flight.DestinationICAO, ScheduledDeparture: *flight.ScheduledDeparture, ActualTakeoff: *flight.ActualTakeoff}
	for batch := range slices.Chunk(streams[2].fixes, 32) {
		report.Positions = batch
		if err := store.RecordSWIMPositions(ctx, report, now); err != nil {
			t.Fatal(err)
		}
	}
	for batch := range slices.Chunk(streams[3].fixes, 32) {
		report.Positions = batch
		if err := store.RecordEurocontrolPositions(ctx, report, now); err != nil {
			t.Fatal(err)
		}
	}

	handler := service.Handler()
	path := "/v1/flights/" + flight.ID + "/radar"
	query := url.Values{"provider": {"1"}, "swim": {"1"}, "eurocontrol": {"1"}}
	read := func(credential string) map[string]json.RawMessage {
		t.Helper()
		raw := importTestRequest(t, handler, http.MethodGet, path+"?"+query.Encode(), credential, "", http.StatusOK)
		var result map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	assertPage := func(page map[string]json.RawMessage, s stream, first, count int, spanning bool) {
		t.Helper()
		var got []feeds.Position
		if err := json.Unmarshal(page[s.positions], &got); err != nil || len(got) != count {
			t.Fatalf("%s: got %d fixes, want %d, decode error %v", s.positions, len(got), count, err)
		}
		for i, fix := range got {
			index := first + i
			if spanning {
				index = (i*(fixCount-1) + count - 2) / (count - 1)
			}
			if want := s.fixes[index]; !fix.At.Equal(want.At) || fix.Source != s.source {
				t.Fatalf("%s[%d]: got %v from %s, want %v from %s", s.positions, i, fix.At, fix.Source, want.At, s.source)
			}
		}
		var cursor string
		if err := json.Unmarshal(page[s.cursor], &cursor); err != nil || cursor != got[len(got)-1].At.Format(time.RFC3339Nano) {
			t.Fatalf("%s: cursor %q, decode error %v", s.positions, cursor, err)
		}
	}

	legacy := read(token)
	query.Set("snapshot", "1")
	initial := read(token)
	foreign := read(otherToken)
	for i, s := range streams {
		assertPage(legacy, s, fixCount-256, 256, false)
		assertPage(initial, s, 0, 512, true)
		if string(foreign[s.positions]) != "[]" || foreign[s.revision] != nil || foreign[s.cursor] != nil {
			t.Fatalf("untracked installation received %s data", s.positions)
		}
		query.Set(s.after, s.fixes[i].At.Format(time.RFC3339Nano))
		if i != 1 {
			query.Set(s.revision, "0")
		}
	}
	forward := read(token)
	for i, s := range streams {
		assertPage(forward, s, i+1, 256, false)
	}
	query.Set("receiverRevision", "1")
	query.Set("providerRevision", now.Add(-time.Second).Format(time.RFC3339Nano))
	query.Set("eurocontrolRevision", "1")
	mixed := read(token)
	for i, s := range streams {
		if s.source == feeds.FAASWIM {
			assertPage(mixed, s, i+1, 256, false)
		} else {
			assertPage(mixed, s, 0, 512, true)
		}
	}
	query.Set("swimRevision", "1")
	assertPage(read(token), streams[2], 0, 512, true)

	for _, invalid := range []string{"snapshot=1", "provider=1&snapshot=", "provider=1&snapshot=2", "provider=1&snapshot=1&snapshot=0"} {
		importTestRequest(t, handler, http.MethodGet, path+"?"+invalid, token, "", http.StatusBadRequest)
	}
}

func seedRadarReceiverFixes(t *testing.T, path, id string, fixes []feeds.Position) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, fix := range fixes {
		raw, err := json.Marshal(fix)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "INSERT INTO radar_positions(flight_id,at_ns,data) VALUES(?,?,?)",
			id, fix.At.UnixNano(), string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRadarProviderPagesCachedPositionsIndependently(t *testing.T) {
	service := newImportTestServer(t)
	service.Source = nil // The route must use the store without calling a flight provider.
	ownerToken, owner := newImportTestInstallation(t, service.Store)
	foreignToken, _ := newImportTestInstallation(t, service.Store)
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-time.Minute)
	fixture := feeds.Flight{ID: "radar-provider-fixture", Number: "XY123", Origin: "LAX", Destination: "EWR", Status: "airborne", ObservedAt: now}
	// Include a duplicate at a page boundary and older, out-of-order data.
	firstAtBoundary := feeds.Position{At: base.Add(256 * time.Nanosecond), Latitude: 32, Longitude: -95, Source: "flightaware_aeroapi"}
	telemetry := []feeds.Position{
		firstAtBoundary,
		{At: now, Latitude: 33, Longitude: -94, Source: "operator_readsb"},
		{At: now.Add(2 * time.Minute), Latitude: 34, Longitude: -93, Source: "flightaware_aeroapi"},
	}
	for i := 259; i >= 0; i-- {
		telemetry = append(telemetry, feeds.Position{
			At: base.Add(time.Duration(i) * time.Nanosecond), Latitude: 31, Longitude: -96, Source: "flightaware_aeroapi",
		})
	}
	if err := service.Store.Observe(t.Context(), fixture, telemetry, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.Track(t.Context(), owner, fixture.ID, ""); err != nil {
		t.Fatal(err)
	}
	handler := service.Handler()
	path := "/v1/flights/" + fixture.ID + "/radar"
	get := func(token string, query url.Values) string {
		t.Helper()
		return importTestRequest(t, handler, http.MethodGet, path+"?"+query.Encode(), token, "", http.StatusOK)
	}
	if got := get(ownerToken, nil); got != "{\"positions\":[]}\n" {
		t.Fatalf("legacy receiver response changed: %s", got)
	}
	type page struct {
		Positions         []feeds.Position `json:"positions"`
		Cursor            string           `json:"cursor"`
		ProviderPositions []feeds.Position `json:"providerPositions"`
		ProviderCursor    string           `json:"providerCursor"`
		ProviderRevision  string           `json:"providerRevision"`
	}
	decode := func(raw string) page {
		t.Helper()
		var p page
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	query := url.Values{"provider": {"1"}, "after": {now.Format(time.RFC3339Nano)}}
	initial := decode(get(ownerToken, query))
	if len(initial.ProviderPositions) != 256 || !initial.ProviderPositions[0].At.Equal(base.Add(4*time.Nanosecond)) ||
		initial.ProviderCursor != base.Add(259*time.Nanosecond).Format(time.RFC3339Nano) || initial.ProviderRevision != now.Format(time.RFC3339Nano) ||
		len(initial.Positions) != 0 || initial.Cursor != "" {
		t.Fatalf("unexpected initial radar page: %d provider positions, cursor %q, %d receiver positions, cursor %q",
			len(initial.ProviderPositions), initial.ProviderCursor, len(initial.Positions), initial.Cursor)
	}
	query.Set("providerAfter", base.Format(time.RFC3339Nano))
	first := decode(get(ownerToken, query))
	if len(first.ProviderPositions) != 256 || first.ProviderCursor != firstAtBoundary.At.Format(time.RFC3339Nano) {
		t.Fatalf("first provider page: %d positions, cursor %q", len(first.ProviderPositions), first.ProviderCursor)
	}
	for i, position := range first.ProviderPositions {
		if want := base.Add(time.Duration(i+1) * time.Nanosecond); !position.At.Equal(want) {
			t.Fatalf("provider position %d at %s, want %s", i, position.At, want)
		}
	}
	last := first.ProviderPositions[len(first.ProviderPositions)-1]
	if last.Latitude != firstAtBoundary.Latitude || last.Longitude != firstAtBoundary.Longitude {
		t.Fatalf("duplicate timestamp returned an inconsistent fix: %+v", last)
	}
	query.Set("providerAfter", first.ProviderCursor)
	second := decode(get(ownerToken, query))
	if len(second.ProviderPositions) != 3 || !second.ProviderPositions[0].At.Equal(base.Add(257*time.Nanosecond)) || second.ProviderCursor != initial.ProviderCursor {
		t.Fatalf("unexpected second provider page: %+v", second)
	}
	query.Set("providerAfter", second.ProviderCursor)
	if empty := decode(get(ownerToken, query)); empty.ProviderPositions == nil || len(empty.ProviderPositions) != 0 || empty.ProviderCursor != "" {
		t.Fatalf("cursor advanced without a provider fix: %+v", empty)
	}
	query.Del("providerAfter")
	if foreign := decode(get(foreignToken, query)); foreign.ProviderPositions == nil || len(foreign.ProviderPositions) != 0 || foreign.ProviderCursor != "" || foreign.ProviderRevision != "" {
		t.Fatalf("foreign installation got provider fixes: %+v", foreign)
	}
	if err := service.Store.Untrack(t.Context(), owner, fixture.ID); err != nil {
		t.Fatal(err)
	}
	if removed := decode(get(ownerToken, query)); removed.ProviderPositions == nil || len(removed.ProviderPositions) != 0 || removed.ProviderCursor != "" || removed.ProviderRevision != "" {
		t.Fatalf("removed flight still returns provider fixes: %+v", removed)
	}
	for _, invalid := range []string{"provider=2", "provider=1&providerAfter=bad", "provider=1&providerRevision=bad", "provider=1&after=bad", "providerAfter=2026-01-01T00:00:00Z", "providerRevision=2026-01-01T00:00:00Z", "receiverRevision=0", "provider=1&receiverRevision=-1", "provider=1&receiverRevision=00", "provider=1&receiverRevision=bad"} {
		importTestRequest(t, handler, http.MethodGet, path+"?"+invalid, ownerToken, "", http.StatusBadRequest)
	}
	importTestRequest(t, handler, http.MethodGet, path+"?provider=1", "", "", http.StatusUnauthorized)
	importTestRequest(t, handler, http.MethodGet, "/v1/flights/unknown-flight/radar?provider=1", ownerToken, "", http.StatusNotFound)
}

func TestRadarReceiverRevisionReportsAResetWithoutFixes(t *testing.T) {
	service := newImportTestServer(t)
	token, owner := newImportTestInstallation(t, service.Store)
	other, _ := newImportTestInstallation(t, service.Store)
	now := time.Now().UTC()
	flight := feeds.Flight{ID: "radar-receiver-revision", Number: "XY123", Status: "airborne", TailNumber: "N1AA", ObservedAt: now}
	observe := func() {
		t.Helper()
		if err := service.Store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	observe()
	if err := service.Store.Track(t.Context(), owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	handler := service.Handler()
	query := url.Values{"provider": {"1"}, "after": {now.Format(time.RFC3339Nano)}}
	get := func(credential string) string {
		t.Helper()
		raw := importTestRequest(t, handler, http.MethodGet, "/v1/flights/"+flight.ID+"/radar?"+query.Encode(), credential, "", http.StatusOK)
		var page struct {
			Positions []feeds.Position `json:"positions"`
			Cursor    string           `json:"cursor"`
			Revision  string           `json:"receiverRevision"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil || page.Positions == nil || len(page.Positions) != 0 || page.Cursor != "" {
			t.Fatalf("unexpected receiver page: %s, error = %v", raw, err)
		}
		return page.Revision
	}
	detailRevision := func(credential string) string {
		t.Helper()
		raw := importTestRequest(t, handler, http.MethodGet, "/v1/flights/"+flight.ID, credential, "", http.StatusOK)
		var detail struct {
			Revision string `json:"receiverRevision"`
		}
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			t.Fatal(err)
		}
		return detail.Revision
	}
	initial := get(token)
	if initial == "" || detailRevision(token) != initial {
		t.Fatal("tracked detail and receiver page must share an initial revision")
	}
	query.Set("receiverRevision", initial)
	flight.ObservedAt = flight.ObservedAt.Add(time.Millisecond)
	observe()
	if got := get(token); got != initial {
		t.Fatalf("ordinary observation changed receiver revision to %q from %q", got, initial)
	}
	flight.TailNumber = ""
	flight.ObservedAt = flight.ObservedAt.Add(time.Millisecond)
	observe()
	if got := get(token); got == "" || got == initial || detailRevision(token) != got {
		t.Fatalf("receiver reset failed to advance both routes from %q to %q", initial, got)
	}
	if got := get(other); got != "" || detailRevision(other) != "" {
		t.Fatalf("receiver revision leaked to an untracked installation: %q", got)
	}
}

func TestRadarProviderRevisionReplacesCorrectedAndClearedHistory(t *testing.T) {
	service := newImportTestServer(t)
	token, owner := newImportTestInstallation(t, service.Store)
	now := time.Now().UTC()
	fixture := feeds.Flight{ID: "radar-provider-revision", Number: "XY123", Status: "airborne", ObservedAt: now}
	point := feeds.Position{At: now.Add(-time.Minute), Latitude: 32, Longitude: -95, Source: "flightaware_aeroapi"}
	observe := func(points []feeds.Position) {
		t.Helper()
		if err := service.Store.Observe(t.Context(), fixture, points, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	observe([]feeds.Position{point})
	if err := service.Store.Track(t.Context(), owner, fixture.ID, ""); err != nil {
		t.Fatal(err)
	}
	handler := service.Handler()
	get := func(revision time.Time) ([]feeds.Position, string, string) {
		t.Helper()
		query := url.Values{
			"provider": {"1"}, "providerAfter": {point.At.Format(time.RFC3339Nano)},
			"providerRevision": {revision.Format(time.RFC3339Nano)},
		}
		raw := importTestRequest(t, handler, http.MethodGet, "/v1/flights/"+fixture.ID+"/radar?"+query.Encode(), token, "", http.StatusOK)
		var page struct {
			Positions []feeds.Position `json:"providerPositions"`
			Cursor    string           `json:"providerCursor"`
			Revision  string           `json:"providerRevision"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		return page.Positions, page.Cursor, page.Revision
	}
	if fixes, cursor, revision := get(now); fixes == nil || len(fixes) != 0 || cursor != "" || revision != now.Format(time.RFC3339Nano) {
		t.Fatalf("unchanged provider cache was not skipped: %+v, %q, %q", fixes, cursor, revision)
	}
	point.Latitude = 33
	fixture.ObservedAt = now.Add(time.Nanosecond)
	observe([]feeds.Position{point})
	if fixes, cursor, revision := get(now); len(fixes) != 1 || fixes[0].Latitude != point.Latitude ||
		cursor != point.At.Format(time.RFC3339Nano) || revision != fixture.ObservedAt.Format(time.RFC3339Nano) {
		t.Fatalf("corrected position at the same cursor was not returned: %+v, %q, %q", fixes, cursor, revision)
	}
	previous := fixture.ObservedAt
	fixture.ObservedAt = now.Add(2 * time.Nanosecond)
	observe([]feeds.Position{})
	if fixes, cursor, revision := get(previous); fixes == nil || len(fixes) != 0 || cursor != "" || revision != fixture.ObservedAt.Format(time.RFC3339Nano) {
		t.Fatalf("cleared cache was not reported as a new revision: %+v, %q, %q", fixes, cursor, revision)
	}
	previous = fixture.ObservedAt
	fixture.ObservedAt = now.Add(3 * time.Nanosecond)
	history := make([]feeds.Position, 520)
	for i := range history {
		history[i] = point
		history[i].At = point.At.Add(time.Duration(i-len(history)+1) * time.Second)
	}
	observe(history)
	if fixes, cursor, revision := get(previous); len(fixes) != 512 || !fixes[0].At.Equal(history[8].At) ||
		cursor != point.At.Format(time.RFC3339Nano) || revision != fixture.ObservedAt.Format(time.RFC3339Nano) {
		t.Fatalf("replacement should carry the newest 512 fixes, got %d with cursor %q and revision %q", len(fixes), cursor, revision)
	}
}
