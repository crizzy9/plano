package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestBackupRestoresLiveWAL(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	source := filepath.Join(dir, "live ?#.db")
	store, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_autocheckpoint=0; PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	flight := feeds.Flight{ID: "backup-flight", Number: "PL123", Origin: "SFO", Destination: "JFK", ObservedAt: now}
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, flight.ID, "12A"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(source + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("fixture did not leave data in the WAL: %v", err)
	}
	// The destination starts in a separate, empty restore directory.
	destination := filepath.Join(t.TempDir(), "portable ?#.db")
	if err := Backup(ctx, source, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions = %o", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(destination))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected a standalone database, entries=%v, err=%v", entries, err)
	}
	if err := store.Untrack(ctx, owner, flight.ID); err != nil {
		t.Fatalf("source write after backup: %v", err)
	}
	if err := Backup(ctx, source, source+"-journal"); err == nil {
		t.Fatal("accepted an absent SQLite sidecar as destination")
	}
	restored, err := Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restoredOwner, err := restored.Installation(ctx, token)
	if err != nil || restoredOwner != owner {
		t.Fatalf("restore did not retain the installation: %v", err)
	}
	tracked, err := restored.Tracked(ctx, restoredOwner)
	if err != nil || len(tracked) != 1 || tracked[0].ID != flight.ID {
		t.Fatalf("restore did not retain the tracked flight: %v %v", tracked, err)
	}
}

func TestBackupRejectsInvalidSourceAndExistingDestination(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	missing, invalid := filepath.Join(dir, "missing.db"), filepath.Join(dir, "invalid.db")
	destination := filepath.Join(dir, "backup.db")
	if err := os.WriteFile(invalid, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{missing, invalid} {
		if err := Backup(ctx, source, destination); err == nil {
			t.Fatal("invalid source was accepted")
		}
	}
	for _, path := range []string{missing, destination} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected artifact %s: %v", path, err)
		}
	}
	if err := os.Symlink(missing, destination); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, invalid, destination); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing destination was not rejected: %v", err)
	}
	if target, err := os.Readlink(destination); err != nil || target != missing {
		t.Fatalf("destination symlink was modified: %v %v", target, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected staging artifacts: %v %v", entries, err)
	}
}
