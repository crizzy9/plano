package core

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOpenMigratesTripItReceiptsWithoutLosingExactKeys(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "plano.db")
	now := time.Now().UTC()
	date := now.Format(time.DateOnly)
	open := func() *Store {
		t.Helper()
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	store := open()
	token, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTripItSubscription(ctx, owner, []byte("fixture"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE tripit_imported_legs;
CREATE TABLE tripit_imported_legs (
  owner TEXT NOT NULL REFERENCES tripit_subscriptions(owner) ON DELETE CASCADE,
  leg_key TEXT NOT NULL, date TEXT NOT NULL, PRIMARY KEY(owner,leg_key)
);`); err != nil {
		t.Fatal(err)
	}
	const key = "legacy-trip-it-key"
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tripit_imported_legs VALUES(?,?,?)`, owner, key, date); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = open()
	if err := store.RemoveTripItSubscription(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if seen, err := store.TripItImportedLegs(ctx, owner, date); err != nil || !seen[key] || len(seen) != 1 {
		t.Fatalf("migrated receipt was lost when stopping sync: %v, err=%v", seen, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = open()
	if seen, err := store.TripItImportedLegs(ctx, owner, date); err != nil || !seen[key] || len(seen) != 1 {
		t.Fatalf("reopening the migrated database changed receipts: %v, err=%v", seen, err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM installations WHERE id=?`, owner); err != nil {
		t.Fatal(err)
	}
	if seen, err := store.TripItImportedLegs(ctx, owner, date); err != nil || len(seen) != 0 {
		t.Fatalf("deleting the installation left receipts: %v, err=%v", seen, err)
	}
}
