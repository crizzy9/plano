package core

import (
	"path/filepath"
	"testing"
	"time"
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
