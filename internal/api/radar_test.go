package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

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
	for _, invalid := range []string{"provider=2", "provider=1&providerAfter=bad", "provider=1&providerRevision=bad", "provider=1&after=bad", "providerAfter=2026-01-01T00:00:00Z", "providerRevision=2026-01-01T00:00:00Z"} {
		importTestRequest(t, handler, http.MethodGet, path+"?"+invalid, ownerToken, "", http.StatusBadRequest)
	}
	importTestRequest(t, handler, http.MethodGet, path+"?provider=1", "", "", http.StatusUnauthorized)
	importTestRequest(t, handler, http.MethodGet, "/v1/flights/unknown-flight/radar?provider=1", ownerToken, "", http.StatusNotFound)
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
