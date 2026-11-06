package core

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func atfmFixture(t *testing.T) (*Store, string, feeds.Flight, feeds.ATFMSlot, time.Time) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "atfm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Truncate(time.Millisecond)
	scheduled := now.Add(time.Hour)
	takeoff := scheduled.Add(20 * time.Minute)
	flight := feeds.Flight{ID: "UAL123-slot-fixture", Number: "UA123", Source: "flightaware_aeroapi",
		Status: "scheduled", Origin: "SFO", Destination: "EWR", OriginICAO: "KSFO", DestinationICAO: "KEWR",
		ScheduledDeparture: &scheduled, ObservedAt: now.Add(-time.Minute)}
	slot := feeds.ATFMSlot{Version: 1, Source: feeds.FAASWIM, ProviderFlightID: flight.ID, Status: feeds.ATFMAssigned,
		OriginICAO: flight.OriginICAO, DestinationICAO: flight.DestinationICAO, ScheduledDeparture: scheduled,
		ObservedAt: now.Add(-20 * time.Second), TakeoffAt: &takeoff, Reason: "Arrival congestion"}
	owner := slotInstallation(t, store)
	observeATFMFixture(t, store, flight)
	if err := store.Track(t.Context(), owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	return store, owner, flight, slot, now
}

func observeATFMFixture(t *testing.T, store *Store, flight feeds.Flight) {
	t.Helper()
	if err := store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func publishATFMFixture(t *testing.T, store *Store, slot feeds.ATFMSlot, now time.Time) {
	t.Helper()
	if err := store.PublishATFMSlot(t.Context(), slot, now); err != nil {
		t.Fatal(err)
	}
}

func readATFMFixture(t *testing.T, store *Store, owner string, flight feeds.Flight, now time.Time) []ATFMSlotView {
	t.Helper()
	slots, err := store.ATFMSlotsForFlight(t.Context(), owner, flight, now)
	if err != nil {
		t.Fatal(err)
	}
	return slots
}

func claimATFMFixture(t *testing.T, store *Store, now time.Time) []deliveryJob {
	t.Helper()
	jobs, err := store.claim(t.Context(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	var slots []deliveryJob
	for _, job := range jobs {
		if job.message.ATFMRevision != "" {
			slots = append(slots, job)
		}
	}
	return slots
}

func TestATFMSlotExactProviderScopeAndTracking(t *testing.T) {
	store, owner, flight, slot, now := atfmFixture(t)
	other := slotInstallation(t, store)
	alias := flight
	alias.ID = "local-schedule-alias"
	observeATFMFixture(t, store, alias)
	if _, err := store.db.ExecContext(t.Context(), `UPDATE aero_instance_keys SET provider_id=? WHERE flight_id=?`, flight.ID, alias.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(t.Context(), owner, alias.ID, ""); err != nil {
		t.Fatal(err)
	}
	otherFlight := flight
	otherFlight.ID = "UAL123-unrelated-fixture"
	observeATFMFixture(t, store, otherFlight)
	if err := store.Track(t.Context(), owner, otherFlight.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Exact ID fallback supports older databases whose snapshots predate the
	// explicit provider-instance mapping.
	if _, err := store.db.ExecContext(t.Context(), `DELETE FROM aero_instance_keys WHERE flight_id=?`, flight.ID); err != nil {
		t.Fatal(err)
	}

	wrong := slot
	wrong.OriginICAO = "KOAK"
	publishATFMFixture(t, store, wrong, now)
	wrong = slot
	wrong.ScheduledDeparture = wrong.ScheduledDeparture.Add(time.Second)
	publishATFMFixture(t, store, wrong, now)
	wrong = slot
	wrong.ProviderFlightID = "UAL123-missing-fixture"
	publishATFMFixture(t, store, wrong, now)
	publishATFMFixture(t, store, slot, now)

	var rows int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM atfm_slots`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("stored %d slots after scope checks, error %v", rows, err)
	}
	for _, matched := range []feeds.Flight{flight, alias} {
		views := readATFMFixture(t, store, owner, matched, now)
		if len(views) != 1 || !views[0].TakeoffAt.Equal(*slot.TakeoffAt) || views[0].Source != slot.Source || views[0].Reason != slot.Reason {
			t.Fatalf("ATFM view for %s = %+v", matched.ID, views)
		}
	}
	if got := readATFMFixture(t, store, owner, otherFlight, now); len(got) != 0 {
		t.Fatalf("unrelated provider instance received %+v", got)
	}
	if got := readATFMFixture(t, store, other, flight, now); len(got) != 0 {
		t.Fatalf("untracked installation received %+v", got)
	}
	if got := readATFMFixture(t, store, owner, flight, slot.ValidUntil()); len(got) != 0 {
		t.Fatalf("expired slot visible: %+v", got)
	}
}

func TestATFMSlotRevisionWithdrawalAndPrivatePush(t *testing.T) {
	store, owner, flight, slot, now := atfmFixture(t)
	other := slotInstallation(t, store)
	for _, recipient := range []struct{ owner, token string }{{owner, "atfm-owner-token"}, {other, "atfm-other-token"}} {
		if err := store.Register(t.Context(), recipient.owner, Device{Platform: "android", Kind: "alert", Token: recipient.token}, now); err != nil {
			t.Fatal(err)
		}
	}
	publishATFMFixture(t, store, slot, now)
	publishATFMFixture(t, store, slot, now)
	heartbeat := slot
	heartbeat.ObservedAt = slot.ObservedAt.Add(time.Second)
	publishATFMFixture(t, store, heartbeat, now)
	initial := claimATFMFixture(t, store, now)
	if len(initial) != 1 || initial[0].target.Token != "atfm-owner-token" {
		t.Fatalf("initial ATFM jobs = %+v", initial)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var submitted []push.Message
	sender := pushFunc(func(_ context.Context, _ push.Target, message push.Message) error {
		submitted = append(submitted, message)
		return nil
	})
	store.deliver(t.Context(), sender, logger, initial[0])
	if len(submitted) != 1 || submitted[0].ATFMRevision != "" || submitted[0].AlertKind != "faa_edct" {
		t.Fatalf("unchanged refresh or private metadata changed initial alert: %+v", submitted)
	}
	updated := heartbeat
	updated.ObservedAt = heartbeat.ObservedAt.Add(time.Second)
	updated.TakeoffAt = new(slot.TakeoffAt.Add(5 * time.Minute))
	publishATFMFixture(t, store, updated, now)
	claimed := claimATFMFixture(t, store, now)
	if len(claimed) != 1 {
		t.Fatalf("updated ATFM jobs = %+v", claimed)
	}
	withdrawn := updated
	withdrawn.Status, withdrawn.TakeoffAt = feeds.ATFMWithdrawn, nil
	publishATFMFixture(t, store, withdrawn, now)
	publishATFMFixture(t, store, updated, now)
	store.deliver(t.Context(), sender, logger, claimed[0])
	if len(submitted) != 1 {
		t.Fatalf("withdrawn assignment escaped as push: %+v", submitted)
	}
	if got := readATFMFixture(t, store, owner, flight, now); len(got) != 0 {
		t.Fatalf("withdrawn slot visible: %+v", got)
	}
	jobs := claimATFMFixture(t, store, now)
	if len(jobs) != 1 || jobs[0].message.CollapseID == initial[0].message.CollapseID ||
		jobs[0].message.CollapseID == claimed[0].message.CollapseID || jobs[0].message.ATFMRevision == claimed[0].message.ATFMRevision ||
		jobs[0].message.APNsCollapseID != initial[0].message.APNsCollapseID || !jobs[0].message.Timestamp.After(claimed[0].message.Timestamp) {
		t.Fatalf("withdrawal replacement jobs = %+v", jobs)
	}
	store.deliver(t.Context(), sender, logger, jobs[0])
	if len(submitted) != 2 || !strings.Contains(submitted[1].Body, "withdrawn") {
		t.Fatalf("withdrawal push = %+v", submitted)
	}
	var timeline int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM events WHERE json_extract(data,'$.kind')='faa_edct'`).Scan(&timeline); err != nil || timeline != 0 {
		t.Fatalf("private slot alerts entered the flight timeline: %d, %v", timeline, err)
	}
}

func TestATFMSlotRevalidatesProviderAtDispatch(t *testing.T) {
	store, owner, flight, slot, now := atfmFixture(t)
	if err := store.Register(t.Context(), owner, Device{Platform: "ios", Kind: "alert", Token: "atfm-owner"}, now); err != nil {
		t.Fatal(err)
	}
	publishATFMFixture(t, store, slot, now)
	jobs := claimATFMFixture(t, store, now)
	if len(jobs) != 1 {
		t.Fatalf("claimed ATFM jobs = %+v", jobs)
	}
	// Leave the lease in place to test the dispatch check independently of
	// the eager retirement path used when a source observation remaps a flight.
	if _, err := store.db.ExecContext(t.Context(), `UPDATE aero_instance_keys SET provider_id=? WHERE flight_id=?`, "UAL123-other-instance", flight.ID); err != nil {
		t.Fatal(err)
	}
	if got := readATFMFixture(t, store, owner, flight, now); len(got) != 0 {
		t.Fatalf("remapped provider received %+v", got)
	}
	store.deliver(t.Context(), pushFunc(func(context.Context, push.Target, push.Message) error {
		t.Fatal("previous provider instance reached push transport")
		return nil
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), jobs[0])
	var remaining int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM outbox WHERE id=?`, jobs[0].id).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("obsolete push row retained: %d, %v", remaining, err)
	}
}

func TestATFMSlotRetainsKnownScopeRevisionsAcrossUntrack(t *testing.T) {
	store, owner, flight, slot, now := atfmFixture(t)
	publishATFMFixture(t, store, slot, now)
	if err := store.Untrack(t.Context(), owner, flight.ID); err != nil {
		t.Fatal(err)
	}
	updated := slot
	updated.ObservedAt = slot.ObservedAt.Add(time.Second)
	updated.TakeoffAt = new(slot.TakeoffAt.Add(10 * time.Minute))
	publishATFMFixture(t, store, updated, now)
	publishATFMFixture(t, store, slot, now)
	if err := store.Track(t.Context(), owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := readATFMFixture(t, store, owner, flight, now); len(got) != 1 || !got[0].TakeoffAt.Equal(*updated.TakeoffAt) {
		t.Fatalf("retrack did not receive latest assignment: %+v", got)
	}
	if err := store.Untrack(t.Context(), owner, flight.ID); err != nil {
		t.Fatal(err)
	}
	withdrawn := updated
	withdrawn.ObservedAt = updated.ObservedAt.Add(time.Second)
	withdrawn.Status, withdrawn.TakeoffAt = feeds.ATFMWithdrawn, nil
	publishATFMFixture(t, store, withdrawn, now)
	if err := store.Track(t.Context(), owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	publishATFMFixture(t, store, updated, now)
	if got := readATFMFixture(t, store, owner, flight, now); len(got) != 0 {
		t.Fatalf("retrack resurfaced a withdrawn assignment: %+v", got)
	}
}

func TestATFMSlotRetiresOnScopeOrPhaseChange(t *testing.T) {
	for _, transition := range []struct {
		name   string
		change func(*feeds.Flight)
	}{
		{"schedule", func(f *feeds.Flight) { f.ScheduledDeparture = new(f.ScheduledDeparture.Add(time.Hour)) }},
		{"destination", func(f *feeds.Flight) { f.DestinationICAO = "KJFK" }},
		{"takeoff", func(f *feeds.Flight) { f.Status, f.ActualTakeoff = "airborne", &f.ObservedAt }},
	} {
		t.Run(transition.name, func(t *testing.T) {
			store, owner, flight, slot, now := atfmFixture(t)
			if err := store.Register(t.Context(), owner, Device{Platform: "android", Kind: "alert", Token: "atfm-owner"}, now); err != nil {
				t.Fatal(err)
			}
			flight.Status, flight.ActualDeparture = "taxi_out", &flight.ObservedAt
			flight.ObservedAt = slot.ObservedAt.Add(-time.Second)
			observeATFMFixture(t, store, flight)
			publishATFMFixture(t, store, slot, now)
			if got := readATFMFixture(t, store, owner, flight, now); len(got) != 1 {
				t.Fatalf("taxiing flight lost its slot: %+v", got)
			}
			jobs := claimATFMFixture(t, store, now)
			if len(jobs) != 1 {
				t.Fatalf("initial slot jobs = %+v", jobs)
			}
			changed := flight
			changed.ObservedAt = slot.ObservedAt.Add(time.Second)
			transition.change(&changed)
			observeATFMFixture(t, store, changed)
			if got := readATFMFixture(t, store, owner, flight, now); len(got) != 0 {
				t.Fatalf("older flight snapshot received %+v", got)
			}
			if got := readATFMFixture(t, store, owner, changed, now); len(got) != 0 {
				t.Fatalf("changed flight received %+v", got)
			}
			store.deliver(t.Context(), pushFunc(func(context.Context, push.Target, push.Message) error {
				t.Fatal("stale slot reached push transport")
				return nil
			}), slog.New(slog.NewTextHandler(io.Discard, nil)), jobs[0])
			flight.ObservedAt = changed.ObservedAt.Add(time.Second)
			observeATFMFixture(t, store, flight)
			if got := readATFMFixture(t, store, owner, flight, now); len(got) != 0 {
				t.Fatalf("retired scope reappeared after reversion: %+v", got)
			}
			slot.ObservedAt = flight.ObservedAt.Add(time.Second)
			publishATFMFixture(t, store, slot, now)
			if got := readATFMFixture(t, store, owner, flight, now); len(got) != 1 {
				t.Fatalf("new source observation did not restore current slot: %+v", got)
			}
		})
	}
}
