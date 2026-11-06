package core

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestRestoreClearsSharingAndRetainsPersonalAlerts(t *testing.T) {
	ctx := t.Context()
	livePath := filepath.Join(t.TempDir(), "live.db")
	live := openRestoreFixture(t, livePath)
	now := time.Now().UTC()
	flight := feeds.Flight{ID: "restore-flight", Number: "PL123", Origin: "SFO", Destination: "JFK", Status: "scheduled", ObservedAt: now}
	if err := live.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	type installation struct {
		owner, token, device, seat string
	}
	owners := []installation{
		{device: "restore-sharer", seat: "12A"},
		{device: "restore-friend-only"},
		{device: "restore-dual", seat: "18C"},
	}
	for i := range owners {
		owner := &owners[i]
		var err error
		owner.token, err = live.CreateInstallation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		owner.owner, err = live.Installation(ctx, owner.token)
		if err != nil {
			t.Fatal(err)
		}
		if owner.seat != "" {
			if err := live.Track(ctx, owner.owner, flight.ID, owner.seat); err != nil {
				t.Fatal(err)
			}
		}
		device := Device{Platform: "ios", Kind: "alert", Token: owner.device}
		if err := live.Register(ctx, owner.owner, device, now); err != nil {
			t.Fatal(err)
		}
	}
	sharer := owners[0].owner
	var friends []Friend
	for _, viewer := range owners[1:] {
		invite, err := live.CreateFriendInvite(ctx, sharer, "Sharer", now)
		if err != nil {
			t.Fatal(err)
		}
		friend, err := live.AcceptFriendInvite(ctx, viewer.owner, invite.Code, "Viewer", now)
		if err != nil {
			t.Fatal(err)
		}
		if err := live.GrantFriendFlight(ctx, sharer, friend.ID, flight.ID); err != nil {
			t.Fatal(err)
		}
		if err := live.SetFriendAlertMode(ctx, viewer.owner, friend.ID, FriendAlertAll); err != nil {
			t.Fatal(err)
		}
		friends = append(friends, friend)
	}
	const unusedAlias = "restore-fixture-unused-invitation-alias"
	unusedInvite, err := live.CreateFriendInvite(ctx, sharer, unusedAlias, now)
	if err != nil {
		t.Fatal(err)
	}
	link, pickupToken, err := live.CreatePickupLink(ctx, sharer, flight.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	flight.DepartureGate = "B12"
	observeFlightAlertFixture(t, live, &flight)
	assertRestoreQueue(t, live, 3, 2, 1)

	snapshot := filepath.Join(t.TempDir(), "backup ?#.db")
	if err := Backup(ctx, livePath, snapshot); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(before, []byte(unusedAlias)) {
		t.Fatal("fixture backup did not contain the invite alias")
	}
	// The live database has revocations that happened after the snapshot.
	if err := live.RevokePickupLink(ctx, sharer, link.ID); err != nil {
		t.Fatal(err)
	}
	for _, friend := range friends {
		if err := live.RemoveFriend(ctx, sharer, friend.ID); err != nil {
			t.Fatal(err)
		}
	}

	destination := filepath.Join(t.TempDir(), "restored ?#.db")
	if err := Restore(ctx, snapshot, destination); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(snapshot)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("restore changed the backup: %v", err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("restored database permissions = %o", info.Mode().Perm())
	}
	if contents, err := os.ReadFile(destination); err != nil || bytes.Contains(contents, []byte(unusedAlias)) {
		t.Fatalf("restored file retained the invite alias or could not be read: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(destination))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(destination) {
		t.Fatalf("restore did not publish one standalone file: %v, %v", entries, err)
	}

	restored := openRestoreFixture(t, destination)
	if _, _, err := restored.SharedPickup(ctx, pickupToken, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restored pickup capability: %v", err)
	}
	if links, err := restored.PickupLinks(ctx, sharer, flight.ID, now); err != nil || len(links) != 0 {
		t.Fatalf("restored pickup links = %d, error = %v", len(links), err)
	}
	if _, err := restored.AcceptFriendInvite(ctx, owners[1].owner, unusedInvite.Code, "Viewer", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restored invitation: %v", err)
	}
	for _, original := range owners {
		owner, err := restored.Installation(ctx, original.token)
		if err != nil || owner != original.owner {
			t.Fatalf("installation was not preserved for fixture %s: %v", original.device, err)
		}
		view, err := restored.Friends(ctx, owner)
		if err != nil || len(view.Friends) != 0 || len(view.Flights) != 0 {
			t.Fatalf("restored relationship or grant for fixture %s: %v", original.device, err)
		}
		tracked, err := restored.Tracked(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		if original.seat == "" {
			if len(tracked) != 0 {
				t.Fatalf("friend-only viewer acquired %d tracked flights", len(tracked))
			}
			continue
		}
		var seat string
		err = restored.db.QueryRowContext(ctx, `SELECT seat FROM tracked WHERE owner=? AND flight_id=?`, owner, flight.ID).Scan(&seat)
		if err != nil || len(tracked) != 1 || tracked[0].ID != flight.ID || seat != original.seat {
			t.Fatalf("tracking was not preserved for fixture %s: %v", original.device, err)
		}
	}
	assertRestoreQueue(t, restored, 2, 0, 0)
	var devices []string
	for _, job := range claimFlightAlertJobs(t, restored, 2) {
		devices = append(devices, job.target.Token)
	}
	slices.Sort(devices)
	if !slices.Equal(devices, []string{"restore-dual", "restore-sharer"}) {
		t.Fatalf("restored alert recipients = %v", devices)
	}
}

func TestRestoreRefusesOccupiedDestination(t *testing.T) {
	dir := t.TempDir()
	livePath, snapshot := filepath.Join(dir, "live.db"), filepath.Join(dir, "backup.db")
	openRestoreFixture(t, livePath)
	if err := Backup(t.Context(), livePath, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		name := suffix
		if name == "" {
			name = "database"
		}
		t.Run(name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "restored.db")
			existing := destination + suffix
			const sentinel = "existing file"
			if err := os.WriteFile(existing, []byte(sentinel), 0600); err != nil {
				t.Fatal(err)
			}
			if err := Restore(t.Context(), snapshot, destination); !errors.Is(err, os.ErrExist) {
				t.Fatalf("occupied destination restore = %v", err)
			}
			if contents, err := os.ReadFile(existing); err != nil || string(contents) != sentinel {
				t.Fatalf("restore overwrote an existing file: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(destination))
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(existing) {
				t.Fatalf("failed restore left artifacts: %v, %v", entries, err)
			}
		})
	}
	t.Run("dangling destination symlink", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "restored.db")
		missing := destination + ".missing"
		if err := os.Symlink(missing, destination); err != nil {
			t.Fatal(err)
		}
		if err := Restore(t.Context(), snapshot, destination); !errors.Is(err, os.ErrExist) {
			t.Fatalf("dangling destination symlink restore = %v", err)
		}
		if target, err := os.Readlink(destination); err != nil || target != missing {
			t.Fatalf("restore changed the destination symlink: %v", err)
		}
		if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restore wrote through the destination symlink: %v", err)
		}
	})
}

func TestRestoreRejectsInvalidSnapshotWithoutPublishing(t *testing.T) {
	snapshot := filepath.Join(t.TempDir(), "invalid.db")
	if err := os.WriteFile(snapshot, []byte("invalid fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Restore(t.Context(), snapshot, filepath.Join(dir, "restored.db")); err == nil {
		t.Fatal("restore accepted an invalid snapshot")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("failed restore left artifacts: %v, %v", entries, err)
	}
}

func TestRestoreAcceptsBackupBeforeSharingMetadata(t *testing.T) {
	ctx := t.Context()
	livePath := filepath.Join(t.TempDir(), "legacy.db")
	live := openRestoreFixture(t, livePath)
	now := time.Now().UTC()
	flight := feeds.Flight{ID: "restore-legacy", Number: "PL123", ObservedAt: now}
	if err := live.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	owner := friendInstallation(t, live)
	if err := live.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, pickupToken, err := live.CreatePickupLink(ctx, owner, flight.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.db.ExecContext(ctx, `
DROP TABLE pickup_link_meta;
DROP TABLE outbox_friend_personal;
DROP TABLE outbox_friend_grants;
DROP TABLE outbox_friend_jobs;
DROP TABLE friend_alert_preferences;
DROP TABLE friend_flights;
DROP TABLE friend_relationships;
DROP TABLE friend_invites;`); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "legacy-backup.db")
	if err := Backup(ctx, livePath, snapshot); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored.db")
	if err := Restore(ctx, snapshot, destination); err != nil {
		t.Fatal(err)
	}
	restored := openRestoreFixture(t, destination)
	if _, _, err := restored.SharedPickup(ctx, pickupToken, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restored legacy pickup capability: %v", err)
	}
	if tracked, err := restored.Tracked(ctx, owner); err != nil || len(tracked) != 1 || tracked[0].ID != flight.ID {
		t.Fatalf("legacy tracked flight was not preserved: %v", err)
	}
}

func openRestoreFixture(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func assertRestoreQueue(t *testing.T, store *Store, wantPending, wantFriend, wantDual int) {
	t.Helper()
	var pending, friend, dual int
	err := store.db.QueryRowContext(t.Context(), `SELECT
  (SELECT count(*) FROM outbox WHERE done=0),
  (SELECT count(*) FROM outbox_friend_jobs),
  (SELECT count(*) FROM outbox_friend_personal)`).Scan(&pending, &friend, &dual)
	if err != nil || pending != wantPending || friend != wantFriend || dual != wantDual {
		t.Fatalf("queue has %d pending, %d friend-marked and %d dual jobs, want %d/%d/%d: %v",
			pending, friend, dual, wantPending, wantFriend, wantDual, err)
	}
}
