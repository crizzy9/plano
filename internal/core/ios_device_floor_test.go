package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestIOSDeviceFloorFencesLateRegistrationAndPreservesNewerDevices(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "devices.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := slotInstallation(t, store)
	other := slotInstallation(t, store)
	now := time.Now().UTC()
	register := func(owner string, d Device) {
		t.Helper()
		if err := store.Register(ctx, owner, d, now); err != nil {
			t.Fatal(err)
		}
	}
	revoke := func(revision int64) {
		t.Helper()
		if err := store.RemoveIOSDevicesThroughRevision(ctx, owner, revision); err != nil {
			t.Fatal(err)
		}
	}

	legacy := Device{Platform: "ios", Kind: "alert", Token: "legacy-alert"}
	alert := Device{Platform: "ios", Kind: "alert", Token: "latest-alert", LiveRevision: 7}
	start := Device{Platform: "ios", Kind: "live_start", Token: "latest-start", LiveSlot: "start", LiveRevision: 8}
	android := Device{Platform: "android", Kind: "alert", Token: "android-alert", Binding: "android-binding"}
	transferred := Device{Platform: "ios", Kind: "alert", Token: "other-owner-alert"}
	for _, device := range []Device{legacy, alert, start, android} {
		register(owner, device)
	}
	register(other, transferred)
	earlierAlert := alert
	earlierAlert.LiveRevision = 3
	register(owner, earlierAlert)
	revoke(5)

	assertSlotDevice(t, store, legacy.Token, "", "")
	assertSlotDevice(t, store, alert.Token, owner, "")
	assertSlotDevice(t, store, start.Token, owner, "start")
	assertSlotDevice(t, store, android.Token, owner, "")
	assertSlotDevice(t, store, transferred.Token, other, "")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}

	revoke(2)
	register(owner, legacy)
	assertSlotDevice(t, store, legacy.Token, "", "")
	transferred.LiveRevision = 5
	register(owner, transferred)
	assertSlotDevice(t, store, transferred.Token, other, "")
	revoke(9)
	assertSlotDevice(t, store, alert.Token, "", "")
	assertSlotDevice(t, store, start.Token, "", "")
	late := Device{Platform: "ios", Kind: "live_start", Token: "unseen-start", LiveSlot: "start", LiveRevision: 9}
	register(owner, late)
	assertSlotDevice(t, store, late.Token, "", "")
	late.LiveRevision++
	register(owner, late)
	revoke(9)
	assertSlotDevice(t, store, late.Token, owner, "start")
	assertSlotDevice(t, store, android.Token, owner, "")
	assertSlotDevice(t, store, transferred.Token, other, "")
}

func TestIOSPushClockMigrationKeepsNewerLegacyAlertRevision(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "token-clock-migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := t.Context()
	owner := slotInstallation(t, store)
	now := time.Now().UTC()
	legacy := Device{Platform: "ios", Kind: "alert", Token: strings.Repeat("ab", 32), LiveRevision: 12}
	if err := store.Register(ctx, owner, legacy, now); err != nil {
		t.Fatal(err)
	}
	stale := legacy
	stale.Token, stale.LiveRevision, stale.PushClock = strings.ToUpper(legacy.Token), 11, "11111111-1111-4111-8111-111111111111"
	if err := store.Register(ctx, owner, stale, now); err != nil {
		t.Fatal(err)
	}
	var revision int64
	var clocks int
	err = store.db.QueryRowContext(ctx, `SELECT r.revision,(SELECT COUNT(*) FROM ios_push_token_clocks)
FROM devices d JOIN ios_alert_revisions r ON r.device_id=d.id WHERE d.owner=? AND d.token=?`, owner, legacy.Token).Scan(&revision, &clocks)
	if err != nil || revision != legacy.LiveRevision || clocks != 0 {
		t.Fatalf("legacy revision=%d clocks=%d error=%v, want %d and no clock receipt", revision, clocks, err, legacy.LiveRevision)
	}
	clocked := stale
	clocked.LiveRevision = 13
	if err := store.Register(ctx, owner, clocked, now); err != nil {
		t.Fatal(err)
	}
	remove := Device{Platform: "ios", Kind: "alert", Token: clocked.Token}
	if err := store.RemoveDevice(ctx, owner, remove); err != nil {
		t.Fatal(err)
	}
	assertSlotDevice(t, store, legacy.Token, "", "")
}

func TestIOSPushClockFencesTokenHandoffs(t *testing.T) {
	const (
		originalClock  = "11111111-1111-4111-8111-111111111111"
		reinstallClock = "22222222-2222-4222-8222-222222222222"
		binding        = "33333333-3333-4333-8333-333333333333"
	)
	for _, kind := range []string{"alert", "live_start", "live_update"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "token-handoff.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			first, second, reinstalled := slotInstallation(t, store), slotInstallation(t, store), slotInstallation(t, store)
			now := time.Now().UTC()
			token := strings.Repeat("ab", 32)
			device := Device{Platform: "ios", Kind: kind, Token: token, PushClock: originalClock}
			switch kind {
			case "live_start":
				device.LiveSlot, device.Binding = "start", binding
			case "live_update":
				flight := feeds.Flight{ID: "handoff-flight", Status: "scheduled", ObservedAt: now}
				if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				for _, owner := range []string{first, second, reinstalled} {
					if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
						t.Fatal(err)
					}
				}
				device.FlightID, device.LiveSlot, device.Binding = flight.ID, "activity:"+strings.Repeat("a", 64), binding
			}
			register := func(owner string, d Device) {
				t.Helper()
				if err := store.Register(ctx, owner, d, now); err != nil {
					t.Fatal(err)
				}
			}
			assertOwner := func(want string) {
				t.Helper()
				var count int
				var owner string
				err := store.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(owner),'')
FROM devices WHERE platform='ios' AND kind=? AND LOWER(token)=?`, kind, token).Scan(&count, &owner)
				if err != nil || owner != want || want == "" && count != 0 || want != "" && count != 1 {
					t.Fatalf("APNs token owner = %q across %d rows, want %q, error = %v", owner, count, want, err)
				}
			}

			device.LiveRevision = 10
			register(first, device)
			transferred := device
			transferred.Token, transferred.LiveRevision = strings.ToUpper(token), 12
			register(second, transferred)
			assertOwner(second)
			device.LiveRevision = 11
			register(first, device)
			assertOwner(second)
			device.LiveRevision = 13
			register(first, device)
			assertOwner(first)

			fresh := transferred
			fresh.PushClock, fresh.LiveRevision = reinstallClock, 1
			register(reinstalled, fresh)
			assertOwner(reinstalled)
			device.LiveRevision = 14
			register(first, device)
			assertOwner(reinstalled)

			// A different push kind and case-sensitive Android tokens keep their
			// own registration history, even if the token text is the same.
			otherKind := Device{Platform: "ios", Kind: "alert", Token: token, PushClock: originalClock, LiveRevision: 15}
			if kind == "alert" {
				otherKind.Kind, otherKind.LiveSlot, otherKind.Binding = "live_start", "start", binding
			}
			register(second, otherKind)
			android := Device{Platform: "android", Kind: "alert", Token: token}
			register(first, android)
			android.Token = strings.ToUpper(token)
			register(second, android)
			var independent int
			if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE
(platform='ios' AND kind=? AND LOWER(token)=? AND owner=?) OR
(platform='android' AND kind='alert' AND ((token=? AND owner=?) OR (token=? AND owner=?)))`,
				otherKind.Kind, token, second, token, first, android.Token, second).Scan(&independent); err != nil || independent != 3 {
				t.Fatalf("independent registrations = %d, want 3, error = %v", independent, err)
			}
			assertOwner(reinstalled)

			if err := store.RemoveIOSDevicesThroughRevision(ctx, reinstalled, fresh.LiveRevision); err != nil {
				t.Fatal(err)
			}
			assertOwner("")
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			transferred.LiveRevision = 16
			register(second, transferred)
			assertOwner("")
			fresh.LiveRevision++
			register(reinstalled, fresh)
			assertOwner(reinstalled)
		})
	}
}
