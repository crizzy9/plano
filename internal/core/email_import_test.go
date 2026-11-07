package core

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestPartialEmailImportPreservesUntrackAcrossRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "email.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	owner, other := friendInstallation(t, store), friendInstallation(t, store)
	relay, err := store.RotateEmailRelay(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	otherRelay, err := store.RotateEmailRelay(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	observeEmailImportFlights(t, store, "first-flight", "replacement-flight", "onward-flight")
	digest, locator := hash("fixture-message"), hash("fixture-locator")
	wanted := []string{hash("first-leg"), hash("same-flight-leg"), hash("onward-leg")}

	complete, partial, err := store.CommitEmailImport(ctx, owner, digest, relay, locator, wanted, nil)
	if err != nil || complete || partial {
		t.Fatalf("unmatched import = complete %t, partial %t, error %v", complete, partial, err)
	}
	complete, partial, err = store.CommitEmailImport(ctx, owner, digest, relay, locator, wanted, []EmailImportLeg{{wanted[0], "first-flight"}})
	if err != nil || complete || !partial {
		t.Fatalf("initial import = complete %t, partial %t, error %v", complete, partial, err)
	}
	if booked, err := store.BookedFlights(ctx, owner, locator); err != nil || len(booked) != 1 || booked[0].ID != "first-flight" {
		t.Fatalf("initial booking has %d flights, error %v", len(booked), err)
	}
	if err := store.Untrack(ctx, owner, "first-flight"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store = reopened
	if progress, err := store.EmailImportProgress(ctx, owner, digest); err != nil || len(progress) != 1 || !progress[wanted[0]] {
		t.Fatalf("reopened progress has %d legs, error %v", len(progress), err)
	}
	if progress, err := store.EmailImportProgress(ctx, other, digest); err != nil || len(progress) != 0 {
		t.Fatalf("other installation progress has %d legs, error %v", len(progress), err)
	}
	complete, partial, err = store.CommitEmailImport(ctx, owner, digest, relay, locator, wanted, nil)
	if err != nil || complete || !partial {
		t.Fatalf("retry without matches = complete %t, partial %t, error %v", complete, partial, err)
	}
	complete, partial, err = store.CommitEmailImport(ctx, owner, digest, relay, locator, wanted, []EmailImportLeg{
		{wanted[0], "replacement-flight"}, // The consumed leg must keep its initial resolution.
		{wanted[1], "first-flight"},       // A different leg key must not undo untracking.
	})
	if err != nil || complete || !partial {
		t.Fatalf("partial replay = complete %t, partial %t, error %v", complete, partial, err)
	}
	if tracked, err := store.Tracked(ctx, owner); err != nil || len(tracked) != 0 {
		t.Fatalf("partial replay tracked %d flights, error %v", len(tracked), err)
	}
	complete, partial, err = store.CommitEmailImport(ctx, owner, digest, relay, locator, wanted, []EmailImportLeg{{wanted[2], "onward-flight"}})
	if err != nil || !complete || partial {
		t.Fatalf("completed import = complete %t, partial %t, error %v", complete, partial, err)
	}
	if seen, err := store.ImportedEmail(ctx, owner, digest); err != nil || !seen {
		t.Fatalf("complete receipt = %t, error %v", seen, err)
	}
	if progress, err := store.EmailImportProgress(ctx, owner, digest); err != nil || len(progress) != 0 {
		t.Fatalf("complete message retains %d partial receipts, error %v", len(progress), err)
	}
	if booked, err := store.BookedFlights(ctx, owner, locator); err != nil || len(booked) != 1 || booked[0].ID != "onward-flight" {
		t.Fatalf("completed booking has %d flights, error %v", len(booked), err)
	}
	complete, partial, err = store.CommitEmailImport(ctx, other, digest, otherRelay, "", wanted, []EmailImportLeg{{wanted[0], "first-flight"}})
	if err != nil || complete || !partial {
		t.Fatalf("other installation import = complete %t, partial %t, error %v", complete, partial, err)
	}
}

func TestEmailImportRollsBackAndFencesRotatedRelay(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "email.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := friendInstallation(t, store)
	oldRelay, err := store.RotateEmailRelay(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	observeEmailImportFlights(t, store, "first-flight")
	digest := hash("fixture-message")
	wanted := []string{hash("first-leg"), hash("onward-leg")}
	resolved := []EmailImportLeg{{wanted[0], "first-flight"}, {wanted[1], "missing-flight"}}
	if _, _, err := store.CommitEmailImport(ctx, owner, digest, oldRelay, "", wanted, resolved); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing flight error = %v", err)
	}
	if tracked, err := store.Tracked(ctx, owner); err != nil || len(tracked) != 0 {
		t.Fatalf("rollback left %d tracked flights, error %v", len(tracked), err)
	}
	if progress, err := store.EmailImportProgress(ctx, owner, digest); err != nil || len(progress) != 0 {
		t.Fatalf("rollback left %d receipts, error %v", len(progress), err)
	}
	relay, err := store.RotateEmailRelay(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CommitEmailImport(ctx, owner, digest, oldRelay, "", wanted, resolved[:1]); !errors.Is(err, ErrEmailRelayUnauthorized) {
		t.Fatalf("stale relay error = %v", err)
	}
	if _, _, err := store.CommitEmailImport(ctx, owner, digest, relay, "", wanted[:1], resolved[1:]); !errors.Is(err, feeds.ErrInvalidQuery) {
		t.Fatalf("unrequested leg error = %v", err)
	}
	if complete, partial, err := store.CommitEmailImport(ctx, owner, digest, relay, "", wanted[:1], resolved[:1]); err != nil || !complete || partial {
		t.Fatalf("current relay import = complete %t, partial %t, error %v", complete, partial, err)
	}
	if _, _, err := store.CommitEmailImport(ctx, owner, digest, oldRelay, "", wanted[:1], nil); !errors.Is(err, ErrEmailRelayUnauthorized) {
		t.Fatalf("completed replay with stale relay error = %v", err)
	}
}

func TestEmailIdentityBindingPreservesLegacyReceipts(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "email.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, other := friendInstallation(t, store), friendInstallation(t, store)
	relay, err := store.RotateEmailRelay(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	otherRelay, err := store.RotateEmailRelay(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	observeEmailImportFlights(t, store, "first-flight")
	for _, tc := range []struct {
		name                   string
		complete, variantFirst bool
	}{
		{name: "completed", complete: true},
		{name: "partial"},
		{name: "partial after variant binding", variantFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, variant, fingerprint := hash(tc.name+"-legacy"), hash(tc.name+"-variant"), hash(tc.name+"-fingerprint")
			wanted := []string{hash(tc.name + "-first-leg")}
			if !tc.complete {
				wanted = append(wanted, hash(tc.name+"-onward-leg"))
			}
			complete, partial, err := store.CommitEmailImport(ctx, owner, raw, relay, "", wanted, []EmailImportLeg{{wanted[0], "first-flight"}})
			if err != nil || complete != tc.complete || partial == tc.complete {
				t.Fatalf("legacy import = complete %t, partial %t, error %v", complete, partial, err)
			}
			assertBinding := func(bytesDigest, want string) {
				t.Helper()
				got, err := store.BindEmailImportDigest(ctx, owner, relay, bytesDigest, fingerprint)
				if err != nil || got != want {
					t.Fatalf("receipt key = %q, want %q, error %v", got, want, err)
				}
			}
			variantKey := raw
			if tc.variantFirst {
				variantKey = fingerprint
				assertBinding(variant, variantKey)
			}
			assertBinding(raw, raw)
			assertBinding(variant, variantKey)
			if digest, err := store.BindEmailImportDigest(ctx, other, otherRelay, raw, fingerprint); err != nil || digest != fingerprint {
				t.Fatalf("other installation receipt key = %q, error %v", digest, err)
			}
			if err := store.Prune(ctx, time.Now().Add(emailIdentityIdleTTL+time.Minute)); err != nil {
				t.Fatal(err)
			}
			if !tc.variantFirst {
				assertBinding(variant, raw)
			}
		})
	}
}

func TestEmailIdentityBindingConvergesAcrossVariantsAndRelayRotation(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "email.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, other := friendInstallation(t, store), friendInstallation(t, store)
	relay, err := store.RotateEmailRelay(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	otherRelay, err := store.RotateEmailRelay(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := hash("fixture-fingerprint")
	type result struct {
		digest string
		err    error
	}
	results := make(chan result, 2)
	for _, raw := range []string{hash("fixture-first-envelope"), hash("fixture-second-envelope")} {
		go func() {
			digest, err := store.BindEmailImportDigest(ctx, owner, relay, raw, fingerprint)
			results <- result{digest, err}
		}()
	}
	for range cap(results) {
		got := <-results
		if got.err != nil || got.digest != fingerprint {
			t.Fatalf("variant receipt key = %q, error %v", got.digest, got.err)
		}
	}
	if _, err := store.BindEmailImportDigest(ctx, owner, otherRelay, hash("fixture-other-envelope"), fingerprint); !errors.Is(err, ErrEmailRelayUnauthorized) {
		t.Fatalf("other installation relay error = %v", err)
	}
	newRelay, err := store.RotateEmailRelay(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	raw := hash("fixture-third-envelope")
	if _, err := store.BindEmailImportDigest(ctx, owner, relay, raw, fingerprint); !errors.Is(err, ErrEmailRelayUnauthorized) {
		t.Fatalf("stale relay error = %v", err)
	}
	if digest, err := store.BindEmailImportDigest(ctx, owner, newRelay, raw, fingerprint); err != nil || digest != fingerprint {
		t.Fatalf("rotated relay receipt key = %q, error %v", digest, err)
	}
	if err := store.Prune(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var bindings int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM email_import_identities WHERE owner=? AND digest=?`, owner, fingerprint).Scan(&bindings); err != nil || bindings != 4 {
		t.Fatalf("recent bindings = %d, error %v", bindings, err)
	}
	if err := store.Prune(ctx, time.Now().Add(emailIdentityIdleTTL+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM email_import_identities WHERE owner=? AND digest=?`, owner, fingerprint).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("idle bindings = %d, error %v", bindings, err)
	}
}

func TestEmailIdentityBindingKeepsExactReplayAcrossParserRevisions(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "email.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := friendInstallation(t, store)
	relay, err := store.RotateEmailRelay(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	raw, fingerprint := hash("fixture-envelope"), hash("fixture-fingerprint")
	if digest, err := store.BindEmailImportDigest(ctx, owner, relay, raw, ""); err != nil || digest != raw {
		t.Fatalf("unbound envelope with no fingerprint = %q, error %v", digest, err)
	}
	if digest, err := store.BindEmailImportDigest(ctx, owner, relay, raw, fingerprint); err != nil || digest != fingerprint {
		t.Fatalf("initial binding = %q, error %v", digest, err)
	}
	observeEmailImportFlights(t, store, "first-flight")
	wanted := []string{hash("fixture-first-leg"), hash("fixture-onward-leg")}
	if complete, partial, err := store.CommitEmailImport(ctx, owner, fingerprint, relay, "", wanted, []EmailImportLeg{{wanted[0], "first-flight"}}); err != nil || complete || !partial {
		t.Fatalf("initial import = complete %t, partial %t, error %v", complete, partial, err)
	}
	if err := store.Prune(ctx, time.Now().Add(emailIdentityIdleTTL+time.Minute)); err != nil {
		t.Fatal(err)
	}
	revisedFingerprint := hash("fixture-revised-fingerprint")
	for _, revised := range []string{revisedFingerprint, ""} {
		digest, err := store.BindEmailImportDigest(ctx, owner, relay, raw, revised)
		if err != nil || digest != fingerprint {
			t.Fatalf("exact replay with revised fingerprint = %q, error %v", digest, err)
		}
		if progress, err := store.EmailImportProgress(ctx, owner, digest); err != nil || len(progress) != 1 || !progress[wanted[0]] {
			t.Fatalf("exact replay lost its original progress: %v, error %v", progress, err)
		}
	}
	if digest, err := store.BindEmailImportDigest(ctx, owner, relay, hash("fixture-new-envelope"), revisedFingerprint); err != nil || digest != fingerprint {
		t.Fatalf("new envelope after exact parser-upgrade replay = %q, error %v", digest, err)
	}
}

func observeEmailImportFlights(t *testing.T, store *Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		flight := feeds.Flight{ID: id, Number: "AA1", Origin: "SFO", Destination: "JFK", Status: "scheduled", ObservedAt: time.Now()}
		if err := store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
}
