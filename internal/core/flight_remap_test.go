package core

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

const firstRemapProvider, secondRemapProvider = "UAL123-remap-a", "UAL123-remap-b"

func TestVerifiedScheduleAliasRemapSeparatesAlertHistory(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "remap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	scheduled := now.Add(3 * time.Hour)
	alias := scheduleRemapAlias(scheduled)

	observeRemapFixture(t, store, alias, firstRemapProvider, "A1", "D1", scheduled, scheduled, now.Add(-4*time.Second))
	owner := slotInstallation(t, store)
	if err := store.Track(ctx, owner, alias, ""); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"ios", "android"} {
		device := Device{Platform: platform, Kind: "alert", Token: platform + "-remap-fixture"}
		if err := store.Register(ctx, owner, device, now); err != nil {
			t.Fatal(err)
		}
	}
	const checkInBody = "Check check-in availability for UA123 with your airline."
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2), "Check-in reminder|"+checkInBody)

	observeRemapFixture(t, store, alias, firstRemapProvider, "B2", "D1", scheduled, scheduled, now.Add(-3*time.Second))
	oldGateJobs := claimFlightAlertJobs(t, store, 2)
	assertFlightAlertHistory(t, store, alias, "check_in|"+checkInBody, "departure_gate|UA123 · B2")

	// The departure gate remains relevant, so lifecycle pruning alone would
	// leave the original lease valid. The new arrival gate and ETD are baselines.
	estimate := scheduled.Add(20 * time.Minute)
	observeRemapFixture(t, store, alias, secondRemapProvider, "B2", "E2", scheduled, estimate, now.Add(-2*time.Second))
	assertFlightAlertSends(t, store, oldGateJobs)
	assertFlightAlertHistory(t, store, alias, "check_in|"+checkInBody)
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2), "Check-in reminder|"+checkInBody)

	observeRemapFixture(t, store, alias, secondRemapProvider, "C3", "E2", scheduled, estimate.Add(6*time.Minute), now.Add(-time.Second))
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 4), "Departure gate changed|UA123 · C3", "Departure estimate changed|UA123 · 6 min later")
	assertFlightAlertHistory(t, store, alias, "check_in|"+checkInBody, "departure_gate|UA123 · C3", "etd|UA123 · 6 min later")

	observeRemapFixture(t, store, alias, firstRemapProvider, "B2", "D1", scheduled, scheduled, now)
	assertFlightAlertHistory(t, store, alias, "check_in|"+checkInBody)
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2), "Check-in reminder|"+checkInBody)
	var checkIns, gates int
	if err := store.db.QueryRowContext(ctx, `SELECT
COUNT(*) FILTER (WHERE json_extract(data,'$.kind')='check_in'),
COUNT(*) FILTER (WHERE json_extract(data,'$.kind')='departure_gate') FROM events WHERE flight_id=?`, alias).Scan(&checkIns, &gates); err != nil {
		t.Fatal(err)
	}
	if checkIns != 3 || gates != 2 {
		t.Fatalf("stored %d check-ins and %d departure gate events, want 3 and 2", checkIns, gates)
	}
}

func TestVerifiedScheduleAliasRemapRefreshesEstablishedLiveUpdates(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "live-remap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	scheduled := now.Add(30 * time.Minute)
	alias := scheduleRemapAlias(scheduled)
	observeRemapFixture(t, store, alias, firstRemapProvider, "A1", "D1", scheduled, scheduled, now.Add(-8*time.Second))
	owner := slotInstallation(t, store)
	if err := store.Track(ctx, owner, alias, "8A"); err != nil {
		t.Fatal(err)
	}
	for _, device := range []Device{
		{Platform: "ios", Kind: "live_update", Token: "ios-remap-update", FlightID: alias},
		{Platform: "android", Kind: "alert", Token: "android-remap-alert"},
	} {
		if err := store.Register(ctx, owner, device, now.Add(-7*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if jobs := liveJobsAt(t, store, now.Add(-6*time.Second)); len(jobs) != 1 || jobs[0].message.Kind != push.LiveUpdate || jobs[0].target.Platform != "ios" {
		t.Fatalf("iOS update registration queued %+v", jobs)
	}
	observeRemapFixture(t, store, alias, firstRemapProvider, "A1", "D1", scheduled, scheduled, now.Add(-5*time.Second))
	if jobs := liveJobsAt(t, store, now.Add(-5*time.Second)); len(jobs) != 1 || jobs[0].message.Kind != push.LiveStart || jobs[0].target.Platform != "android" {
		t.Fatalf("first Android live observation queued %+v", jobs)
	}
	claimLive := func(at time.Time) []deliveryJob {
		t.Helper()
		jobs, err := store.claim(ctx, at, 10)
		if err != nil {
			t.Fatal(err)
		}
		jobs = slices.DeleteFunc(jobs, func(job deliveryJob) bool { return job.message.Kind == push.Alert })
		if len(jobs) != 2 {
			t.Fatalf("claimed %d live jobs, want 2", len(jobs))
		}
		return jobs
	}
	observeRemapFixture(t, store, alias, firstRemapProvider, "B2", "D1", scheduled, scheduled, now.Add(-4*time.Second))
	old := claimLive(now.Add(-4 * time.Second))
	observeRemapFixture(t, store, alias, secondRemapProvider, "C3", "D1", scheduled, scheduled, now)
	logger := slog.New(slog.DiscardHandler)
	for _, job := range old {
		store.deliver(ctx, pushFunc(func(_ context.Context, target push.Target, _ push.Message) error {
			t.Fatalf("retired live update reached %s", target.Platform)
			return nil
		}), logger, job)
	}
	var delivered []string
	for _, job := range claimLive(now) {
		store.deliver(ctx, pushFunc(func(_ context.Context, target push.Target, msg push.Message) error {
			if msg.Kind != push.LiveUpdate || msg.FlightID != alias || msg.State == nil || msg.State.DepartureGate != "C3" || msg.State.ObservedEpoch != now.Unix() {
				t.Fatalf("unexpected replacement live push to %s: %+v", target.Platform, msg)
			}
			delivered = append(delivered, target.Platform+"|"+target.Token)
			return nil
		}), logger, job)
	}
	slices.Sort(delivered)
	if want := []string{"android|android-remap-alert", "ios|ios-remap-update"}; !slices.Equal(delivered, want) {
		t.Fatalf("replacement live pushes reached %q, want %q", delivered, want)
	}
}

func TestVerifiedScheduleAliasRemapReplacesUnsubmittedLiveStart(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "live-start-remap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	scheduled := now.Add(30 * time.Minute)
	alias := scheduleRemapAlias(scheduled)
	observeRemapFixture(t, store, alias, firstRemapProvider, "A1", "D1", scheduled, scheduled, now.Add(-3*time.Second))
	owner := slotInstallation(t, store)
	if err := store.Track(ctx, owner, alias, ""); err != nil {
		t.Fatal(err)
	}
	device := Device{Platform: "ios", Kind: "live_start", Token: "ios-remap-start"}
	if err := store.Register(ctx, owner, device, now.Add(-2*time.Second)); err != nil {
		t.Fatal(err)
	}
	observeRemapFixture(t, store, alias, firstRemapProvider, "A1", "D1", scheduled, scheduled, now.Add(-time.Second))
	old, err := store.claim(ctx, now.Add(-time.Second), 10)
	if err != nil || len(old) != 1 || old[0].message.Kind != push.LiveStart {
		t.Fatalf("original live start claims = %+v, error = %v", old, err)
	}

	observeRemapFixture(t, store, alias, secondRemapProvider, "B2", "D1", scheduled, scheduled, now)
	store.deliver(ctx, pushFunc(func(context.Context, push.Target, push.Message) error {
		t.Fatal("retired live start reached the transport")
		return nil
	}), slog.New(slog.DiscardHandler), old[0])
	replacement, err := store.claim(ctx, now, 10)
	if err != nil || len(replacement) != 1 || replacement[0].message.Kind != push.LiveStart || replacement[0].target.Token != device.Token ||
		replacement[0].message.State == nil || replacement[0].message.State.DepartureGate != "B2" {
		t.Fatalf("replacement live start claims = %+v, error = %v", replacement, err)
	}
	started := false
	store.deliver(ctx, pushFunc(func(context.Context, push.Target, push.Message) error {
		started = true
		// Once dispatch authorizes a start, a remap preserves the latch so the
		// existing activity can register its update token without a duplicate.
		observeRemapFixture(t, store, alias, firstRemapProvider, "C3", "D1", scheduled, scheduled, now.Add(time.Second))
		return nil
	}), slog.New(slog.DiscardHandler), replacement[0])
	if jobs, err := store.claim(ctx, now.Add(time.Second), 10); !started || err != nil || len(jobs) != 0 {
		t.Fatalf("started = %t, duplicate start claims = %+v, error = %v", started, jobs, err)
	}
}

func scheduleRemapAlias(scheduled time.Time) string {
	return "scheduled_" + base64.RawURLEncoding.EncodeToString([]byte("UA123|UAL123|KSFO|KEWR|"+scheduled.Format(time.RFC3339)))
}

func observeRemapFixture(t *testing.T, store *Store, alias, provider, departureGate, arrivalGate string, scheduled, estimate, observed time.Time) {
	t.Helper()
	source := feeds.NewAeroAPI("fixture-only", &http.Client{Transport: aeroCallbackTransport(func(*http.Request) (*http.Response, error) {
		raw := fmt.Sprintf(`{"flights":[{"fa_flight_id":%q,"ident":"UAL123","ident_iata":"UA123","ident_icao":"UAL123",
"origin":{"code_icao":"KSFO","code_iata":"SFO"},"destination":{"code_icao":"KEWR","code_iata":"EWR"},
"scheduled_out":%q,"estimated_out":%q,"gate_origin":%q,"gate_destination":%q}]}`,
			provider, scheduled.Format(time.RFC3339), estimate.Format(time.RFC3339), departureGate, arrivalGate)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}, nil
	})})
	flight, err := source.Get(t.Context(), alias)
	if err != nil || flight.ID != alias || flight.AeroProviderID() != provider {
		t.Fatalf("resolved flight %q with provider %q, want %q and %q, error %v", flight.ID, flight.AeroProviderID(), alias, provider, err)
	}
	flight.ObservedAt = observed
	if err := store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}
