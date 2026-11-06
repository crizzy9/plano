package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestInboundMilestonesNotifyTrackingInstallations(t *testing.T) {
	store, owner, onward, leg, now := inboundMilestoneFixture(t, true)
	ctx := t.Context()
	viewer := friendInstallation(t, store)
	if err := store.Register(ctx, viewer, Device{Platform: "ios", Kind: "alert", Token: "inbound-friend"}, now); err != nil {
		t.Fatal(err)
	}
	invite, err := store.CreateFriendInvite(ctx, owner, "Traveler", now)
	if err != nil {
		t.Fatal(err)
	}
	friend, err := store.AcceptFriendInvite(ctx, viewer, invite.Code, "Viewer", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.GrantFriendFlight(ctx, owner, friend.ID, onward.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetFriendAlertMode(ctx, viewer, friend.ID, FriendAlertAll); err != nil {
		t.Fatal(err)
	}

	takeoff := now.Add(-3 * time.Minute)
	leg.Status, leg.ActualTakeoff, leg.ObservedAt = "airborne", &takeoff, now.Add(-2*time.Minute)
	for range 2 {
		if err := store.IngestAeroObservation(ctx, leg, now); err != nil {
			t.Fatal(err)
		}
	}
	leg.ObservedAt = leg.ObservedAt.Add(time.Second)
	if err := store.IngestAeroObservation(ctx, leg, now); err != nil {
		t.Fatal(err)
	}
	assertInboundMilestoneSends(t, store, owner, onward.ID, leg.ID, inboundTakeoff, "Inbound aircraft took off", takeoff)

	landing := now.Add(-time.Minute)
	leg.Status, leg.ActualLanding, leg.ObservedAt = "landed", &landing, now.Add(-30*time.Second)
	onward.ObservedAt = now
	if err := store.observeTracked(ctx, onward, nil, []feeds.Flight{leg}, nil); err != nil {
		t.Fatal(err)
	}
	assertInboundMilestoneSends(t, store, owner, onward.ID, leg.ID, inboundLanding, "Inbound aircraft landed", landing)

	if events := inboundMilestoneEventKinds(t, store, owner, onward.ID); !slices.Equal(events, []string{inboundLanding, inboundTakeoff}) {
		t.Fatalf("tracked timeline = %v, want landing and takeoff once each", events)
	}
	if events := inboundMilestoneEventKinds(t, store, viewer, onward.ID); len(events) != 0 {
		t.Fatalf("friend timeline includes private milestones: %v", events)
	}
	if err := store.Untrack(ctx, owner, onward.ID); err != nil {
		t.Fatal(err)
	}
	if events := inboundMilestoneEventKinds(t, store, owner, onward.ID); len(events) != 0 {
		t.Fatalf("timeline after untracking includes private milestones: %v", events)
	}
}

func TestInboundMilestonesRejectUnqualifiedObservations(t *testing.T) {
	for _, tc := range []struct {
		name       string
		noPriorLeg bool
		change     func(*feeds.Flight, *feeds.Flight, time.Time)
	}{
		{name: "first observation", noPriorLeg: true},
		{name: "different linked flight", change: func(_ *feeds.Flight, leg *feeds.Flight, _ time.Time) {
			leg.ID = "other-inbound"
		}},
		{name: "unverified provider leg", change: func(_ *feeds.Flight, leg *feeds.Flight, _ time.Time) {
			leg.Source = "plano_demo"
		}},
		{name: "conflicting tail", change: func(_ *feeds.Flight, leg *feeds.Flight, _ time.Time) {
			leg.TailNumber = "N999AA"
		}},
		{name: "conflicting destination", change: func(_ *feeds.Flight, leg *feeds.Flight, _ time.Time) {
			leg.Destination, leg.DestinationICAO = "BOS", "KBOS"
		}},
		{name: "onward has pushed back", change: func(onward, _ *feeds.Flight, now time.Time) {
			onward.Status, onward.ActualDeparture = "taxi_out", &now
		}},
		{name: "stale takeoff", change: func(_ *feeds.Flight, leg *feeds.Flight, now time.Time) {
			leg.ActualTakeoff = new(now.Add(-time.Hour))
		}},
		{name: "takeoff after observation", change: func(_ *feeds.Flight, leg *feeds.Flight, now time.Time) {
			leg.ActualTakeoff = new(now.Add(-time.Minute))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, owner, onward, leg, now := inboundMilestoneFixture(t, !tc.noPriorLeg)
			leg.Status, leg.ActualTakeoff, leg.ObservedAt = "airborne", new(now.Add(-3*time.Minute)), now.Add(-2*time.Minute)
			onward.ObservedAt = now
			if tc.change != nil {
				tc.change(&onward, &leg, now)
			}
			if err := store.observeTracked(t.Context(), onward, nil, []feeds.Flight{leg}, nil); err != nil {
				t.Fatal(err)
			}
			if events := inboundMilestoneEventKinds(t, store, owner, onward.ID); len(events) != 0 {
				t.Fatalf("unqualified observation added milestones: %v", events)
			}
		})
	}
}

func TestInboundMilestoneTakeoffRechecksAssignmentAndLandingAtSend(t *testing.T) {
	for _, update := range []string{"landing", "reassignment"} {
		t.Run(update, func(t *testing.T) {
			store, _, onward, leg, now := inboundMilestoneFixture(t, true)
			ctx := t.Context()
			leg.Status, leg.ActualTakeoff, leg.ObservedAt = "airborne", new(now.Add(-3*time.Minute)), now.Add(-2*time.Minute)
			if err := store.IngestAeroObservation(ctx, leg, now); err != nil {
				t.Fatal(err)
			}
			jobs := claimFlightAlertJobs(t, store, 2)
			if update == "landing" {
				leg.Status, leg.ActualLanding, leg.ObservedAt = "landed", new(now.Add(-time.Minute)), now.Add(-30*time.Second)
				if err := store.IngestAeroObservation(ctx, leg, now); err != nil {
					t.Fatal(err)
				}
			} else {
				onward.InboundID, onward.ObservedAt = "new-assignment", now
				if err := store.observeTracked(ctx, onward, nil, []feeds.Flight{}, nil); err != nil {
					t.Fatal(err)
				}
			}
			for _, job := range jobs {
				if job.message.InboundLegID != leg.ID {
					t.Fatalf("claimed takeoff lacks expected private evidence: %+v", job.message)
				}
			}
			assertFlightAlertSends(t, store, jobs)
		})
	}
}

func inboundMilestoneFixture(t *testing.T, cacheInbound bool) (*Store, string, feeds.Flight, feeds.Flight, time.Time) {
	t.Helper()
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "inbound-milestones.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	onward := feeds.Flight{ID: "onward", Number: "AA2", Source: "flightaware_aeroapi", Status: "scheduled", InboundID: "inbound",
		Origin: "JFK", OriginICAO: "KJFK", Destination: "BOS", DestinationICAO: "KBOS", TailNumber: "N123AA",
		ScheduledDeparture: new(now.Add(24*time.Hour + 15*time.Minute)), ObservedAt: now.Add(-5 * time.Minute)}
	leg := feeds.Flight{ID: onward.InboundID, Number: "AA1", Source: onward.Source, Status: "scheduled",
		Origin: "LAX", OriginICAO: "KLAX", Destination: onward.Origin, DestinationICAO: onward.OriginICAO, TailNumber: onward.TailNumber,
		ScheduledDeparture: new(now.Add(-4 * time.Minute)), ScheduledArrival: new(now.Add(2 * time.Hour)), ObservedAt: now.Add(-5 * time.Minute)}
	var rotation []feeds.Flight
	if cacheInbound {
		rotation = []feeds.Flight{leg}
	}
	if err := store.Observe(ctx, onward, nil, rotation, nil); err != nil {
		t.Fatal(err)
	}
	owner := friendInstallation(t, store)
	if err := store.Track(ctx, owner, onward.ID, ""); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"ios", "android"} {
		if err := store.Register(ctx, owner, Device{Platform: platform, Kind: "alert", Token: "inbound-owner-" + platform}, now); err != nil {
			t.Fatal(err)
		}
	}
	return store, owner, onward, leg, now
}

func assertInboundMilestoneSends(t *testing.T, store *Store, owner, flightID, legID, kind, title string, occurredAt time.Time) {
	t.Helper()
	var platforms []string
	sender := pushFunc(func(_ context.Context, target push.Target, msg push.Message) error {
		wantKind := ""
		if target.Platform == "android" {
			wantKind = kind
		}
		if target.Token != "inbound-owner-"+target.Platform || msg.Kind != push.Alert || msg.Title != title || msg.FlightID != flightID ||
			msg.AlertKind != wantKind || msg.InboundLegID != "" || !msg.Timestamp.Equal(occurredAt) ||
			!msg.ExpiresAt.Equal(occurredAt.Add(inboundMilestoneLifetime)) {
			t.Fatalf("unexpected milestone delivery to %s: %+v", target.Token, msg)
		}
		platforms = append(platforms, target.Platform)
		return nil
	})
	logger := slog.New(slog.DiscardHandler)
	for _, job := range claimFlightAlertJobs(t, store, 2) {
		if job.owner != owner || job.message.InboundLegID != legID {
			t.Fatalf("unexpected milestone claim for %s: %+v", job.owner, job.message)
		}
		store.deliver(t.Context(), sender, logger, job)
	}
	slices.Sort(platforms)
	if !slices.Equal(platforms, []string{"android", "ios"}) {
		t.Fatalf("delivered platforms = %v, want Android and iOS", platforms)
	}
}

func inboundMilestoneEventKinds(t *testing.T, store *Store, owner, id string) []string {
	t.Helper()
	details, err := store.DetailsForInstallation(t.Context(), owner, id)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, event := range details.Events {
		if event.Kind == inboundTakeoff || event.Kind == inboundLanding {
			kinds = append(kinds, event.Kind)
		}
	}
	return kinds
}
