package core

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/push"
)

func TestFriendAlertsFollowOriginalGrantsAndPreferences(t *testing.T) {
	store, sharer, flightID := dispatcherFixture(t)
	ctx := t.Context()
	flight, err := store.Flight(ctx, flightID)
	if err != nil {
		t.Fatal(err)
	}
	otherSharer := friendInstallation(t, store)
	all, arrival, personal, muted := friendInstallation(t, store), friendInstallation(t, store), friendInstallation(t, store), friendInstallation(t, store)
	for _, owner := range []string{sharer, otherSharer, personal} {
		if err := store.Track(ctx, owner, flightID, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ owner, platform, token string }{
		{sharer, "ios", "owner-fixture"},
		{all, "android", "all-fixture"},
		{arrival, "ios", "arrival-fixture"},
		{personal, "android", "personal-fixture"},
		{muted, "android", "muted-fixture"},
	} {
		if err := store.Register(ctx, item.owner, Device{Platform: item.platform, Kind: "alert", Token: item.token}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	share := func(owner, viewer, mode string) Friend {
		t.Helper()
		now := time.Now()
		invite, err := store.CreateFriendInvite(ctx, owner, "Sharer", now)
		if err != nil {
			t.Fatal(err)
		}
		friend, err := store.AcceptFriendInvite(ctx, viewer, invite.Code, "Viewer", now)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.GrantFriendFlight(ctx, owner, friend.ID, flightID); err != nil {
			t.Fatal(err)
		}
		if mode != "" {
			if err := store.SetFriendAlertMode(ctx, viewer, friend.ID, mode); err != nil {
				t.Fatal(err)
			}
		}
		return friend
	}
	first := share(sharer, all, FriendAlertAll)
	second := share(otherSharer, all, FriendAlertAll)
	share(sharer, arrival, FriendAlertArrival)
	share(sharer, personal, FriendAlertAll)
	share(sharer, muted, "")

	assertSends := func(jobs []deliveryJob, title string, want ...string) {
		t.Helper()
		var sent []string
		sender := pushFunc(func(_ context.Context, target push.Target, msg push.Message) error {
			if msg.Kind != push.Alert || msg.FlightID != flightID || msg.Title != title || msg.State != nil {
				t.Fatalf("unexpected push to %s: %+v", target.Token, msg)
			}
			sent = append(sent, target.Token)
			return nil
		})
		for _, job := range jobs {
			store.deliver(ctx, sender, slog.New(slog.DiscardHandler), job)
		}
		slices.Sort(sent)
		slices.Sort(want)
		if !slices.Equal(sent, want) {
			t.Fatalf("%s sent to %v, want %v", title, sent, want)
		}
	}
	gateChange := func(gate string) []deliveryJob {
		t.Helper()
		flight.DepartureGate = gate
		observeFlightAlertFixture(t, store, &flight)
		return claimFlightAlertJobs(t, store, 3)
	}
	jobs := gateChange("B2")
	if err := store.SetFriendAlertMode(ctx, all, second.ID, FriendAlertAll); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeFriendFlight(ctx, sharer, first.ID, flightID); err != nil {
		t.Fatal(err)
	}
	assertSends(jobs, "Departure gate", "all-fixture", "owner-fixture", "personal-fixture")

	jobs = gateChange("C3")
	if err := store.RevokeFriendFlight(ctx, otherSharer, second.ID, flightID); err != nil {
		t.Fatal(err)
	}
	if err := store.GrantFriendFlight(ctx, otherSharer, second.ID, flightID); err != nil {
		t.Fatal(err)
	}
	assertSends(jobs, "Departure gate changed", "owner-fixture", "personal-fixture")

	jobs = gateChange("D4")
	for _, mode := range []string{FriendAlertOff, FriendAlertAll} {
		if err := store.SetFriendAlertMode(ctx, all, second.ID, mode); err != nil {
			t.Fatal(err)
		}
	}
	assertSends(jobs, "Departure gate changed", "owner-fixture", "personal-fixture")

	flight.Status = "landed"
	observeFlightAlertFixture(t, store, &flight)
	assertSends(claimFlightAlertJobs(t, store, 4), "Landed", "all-fixture", "arrival-fixture", "owner-fixture", "personal-fixture")
	if flights, err := store.Tracked(ctx, all); err != nil || len(flights) != 0 {
		t.Fatalf("friend alerts changed the viewer's own flight list, flights=%d, error=%v", len(flights), err)
	}
}
