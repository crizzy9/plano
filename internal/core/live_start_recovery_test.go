package core

import (
	"context"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/push"
)

func TestLiveStartRecoveryClassifiesSendOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, platform string
		sendErr        error
		retry          bool
	}{
		{name: "iOS deadline", platform: "ios", sendErr: context.DeadlineExceeded},
		{name: "iOS lost response", platform: "ios", sendErr: &push.DeliveryError{Provider: "APNs", Code: "TransportError", Retryable: true}},
		{name: "iOS HTTP rejection", platform: "ios", sendErr: &push.DeliveryError{Provider: "APNs", Status: 429, Retryable: true}, retry: true},
		{name: "Android lost response", platform: "android", sendErr: &push.DeliveryError{Provider: "FCM", Code: "TransportError", Retryable: true}, retry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newLiveStartDispatchFixture(t, tc.platform)
			first := fixture.claimStart(t, fixture.now.Add(time.Second))
			fixture.deliver(t, first, tc.sendErr)
			if len(fixture.sent) != 1 {
				t.Fatalf("first start did not reach transport: %+v", fixture.sent)
			}
			later := time.Now().Add(2 * time.Second)
			if tc.retry {
				retry := fixture.claimStart(t, later)
				if retry.id != first.id || retry.attempts != 1 {
					t.Fatalf("expected the same start to be retried, got %+v", retry)
				}
				fixture.deliver(t, retry, nil)
				if len(fixture.sent) != 2 {
					t.Fatalf("retry did not reach transport: %+v", fixture.sent)
				}
				return
			}
			fixture.observe(t, later.Sub(fixture.now))
			if jobs := fixture.claimLive(t, later); len(jobs) != 0 {
				t.Fatalf("start with a possibly accepted result was queued again: %+v", jobs)
			}
		})
	}
}

func TestLiveStartRecoveryDoesNotResubmitAbandonedAuthorization(t *testing.T) {
	fixture := newLiveStartDispatchFixture(t, "ios")
	first := fixture.claimStart(t, fixture.now.Add(time.Second))
	if authorized, err := fixture.store.authorizeLiveStart(t.Context(), &first, fixture.now.Add(2*time.Second)); err != nil || !authorized {
		t.Fatalf("authorize first start: authorized=%t, err=%v", authorized, err)
	}
	// The process can disappear after submitting the start and before storing
	// a response. Let the original lease expire with the outbox row intact.
	recoverAt := time.Unix(0, first.lease).Add(time.Second)
	for _, job := range fixture.claimLive(t, recoverAt) {
		fixture.deliver(t, job, nil)
	}
	if len(fixture.sent) != 0 {
		t.Fatalf("recovery resubmitted an authorized iOS start: %+v", fixture.sent)
	}
	fixture.observe(t, recoverAt.Sub(fixture.now))
	if jobs := fixture.claimLive(t, time.Unix(0, first.lease).Add(time.Minute)); len(jobs) != 0 {
		t.Fatalf("abandoned authorization remained eligible for dispatch: %+v", jobs)
	}
}

func TestLiveStartRecoveryPreservesAuthorizationAcrossTokenRotation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prune bool
	}{
		{name: "pending authorization"},
		{name: "expired authorization", prune: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newLiveStartDispatchFixture(t, "ios")
			first := fixture.claimStart(t, fixture.now.Add(time.Second))
			at := fixture.now.Add(2 * time.Second)
			if authorized, err := fixture.store.authorizeLiveStart(t.Context(), &first, at); err != nil || !authorized {
				t.Fatalf("authorize first start: authorized=%t, err=%v", authorized, err)
			}
			if tc.prune {
				at = first.message.ExpiresAt.Add(time.Second)
				if err := fixture.store.Prune(t.Context(), at); err != nil {
					t.Fatal(err)
				}
				fixture.observe(t, at.Sub(fixture.now))
				if jobs := fixture.claimLive(t, at); len(jobs) != 0 {
					t.Fatalf("prune allowed another start on the same token: %+v", jobs)
				}
			}
			replacement := fixture.device
			replacement.Token, replacement.LiveRevision = "replacement-start-token", fixture.device.LiveRevision+1
			if err := fixture.store.Register(t.Context(), fixture.owner, replacement, at.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			later := at.Add(2 * time.Second)
			fixture.observe(t, later.Sub(fixture.now))
			if jobs := fixture.claimLive(t, later); len(jobs) != 0 {
				t.Fatalf("rotated token could repeat a possibly accepted start: %+v", jobs)
			}
		})
	}
}

func TestLiveStartRecoveryRequeuesExpiredUnsubmittedStart(t *testing.T) {
	fixture := newLiveStartDispatchFixture(t, "ios")
	first := fixture.claimStart(t, fixture.now.Add(time.Second))
	later := first.message.ExpiresAt.Add(time.Second)
	if err := fixture.store.Prune(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	fixture.observe(t, later.Sub(fixture.now))
	replacement := fixture.claimStart(t, later)
	if replacement.id == first.id {
		t.Fatal("expected the expired unsubmitted job to be replaced")
	}
	fixture.deliver(t, replacement, nil)
	if len(fixture.sent) != 1 {
		t.Fatalf("replacement did not reach transport: %+v", fixture.sent)
	}
}

func TestLiveStartRecoveryBackfillsLegacyLeaseAndSurvivesInvalidation(t *testing.T) {
	fixture := newLiveStartDispatchFixture(t, "ios")
	ctx := t.Context()
	first := fixture.claimStart(t, fixture.now.Add(time.Second))
	// A database from before submission tracking has the active lease only.
	if _, err := fixture.store.db.ExecContext(ctx, `DELETE FROM outbox_live_start_leases WHERE outbox_id=?`, first.id); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := fixture.store.db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	fixture.store = reopened
	// APNs can invalidate the same token in response to a different flight.
	// The binding's start history must survive deletion of the device row.
	invalidated := deliveryJob{deviceID: first.deviceID, registered: first.registered}
	if err := reopened.resolve(ctx, invalidated, &push.DeliveryError{Provider: "APNs", Status: 410, InvalidToken: true}, fixture.now); err != nil {
		t.Fatal(err)
	}
	replacement := fixture.device
	replacement.Token, replacement.LiveRevision = "replacement-after-invalidation", 2
	if err := reopened.Register(ctx, fixture.owner, replacement, fixture.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.observe(t, 4*time.Second)
	if jobs := fixture.claimLive(t, fixture.now.Add(5*time.Second)); len(jobs) != 0 {
		t.Fatalf("replacement token could repeat a possibly accepted legacy start: %+v", jobs)
	}
}

func TestLiveStartRecoveryRetriesAfterRejectionDuringTokenRotation(t *testing.T) {
	fixture := newLiveStartDispatchFixture(t, "ios")
	ctx := t.Context()
	first := fixture.claimStart(t, fixture.now.Add(time.Second))
	if authorized, err := fixture.store.authorizeLiveStart(ctx, &first, fixture.now.Add(2*time.Second)); err != nil || !authorized {
		t.Fatalf("authorize first start: authorized=%t, err=%v", authorized, err)
	}
	replacement := fixture.device
	replacement.Token, replacement.LiveRevision = "replacement-before-rejection", 2
	if err := fixture.store.Register(ctx, fixture.owner, replacement, fixture.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.resolve(ctx, first, &push.DeliveryError{Provider: "APNs", Status: 400}, fixture.now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.observe(t, 5*time.Second)
	jobs := fixture.claimLive(t, fixture.now.Add(6*time.Second))
	if len(jobs) != 1 || jobs[0].id == first.id || jobs[0].message.Kind != push.LiveStart || jobs[0].target.Token != replacement.Token {
		t.Fatalf("definite rejection did not release the old attempt: %+v", jobs)
	}
}
