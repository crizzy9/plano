package core

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func friendInstallation(t *testing.T, s *Store) string {
	t.Helper()
	token, err := s.CreateInstallation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Installation(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFriendInviteExpiresAndConcurrentAcceptUsesItOnce(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "friends.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	owner, viewer, other := friendInstallation(t, s), friendInstallation(t, s), friendInstallation(t, s)
	now := time.Date(2026, time.November, 5, 23, 20, 0, 0, time.UTC)
	invite, err := s.CreateFriendInvite(ctx, owner, "  الشارِك ✈  ", now)
	if err != nil || !invite.ExpiresAt.Equal(now.Add(24*time.Hour)) || !friendInviteCodeRE.MatchString(invite.Code) {
		t.Fatalf("invalid invite or TTL: %v", err)
	}
	var stored, alias string
	if err := s.db.QueryRowContext(ctx, `SELECT token_hash,sharer_alias FROM friend_invites`).Scan(&stored, &alias); err != nil || stored != hash(invite.Code) || stored == invite.Code || alias != "الشارِك ✈" {
		t.Fatalf("invite persistence did not hash the code or trim the alias: %v", err)
	}
	if _, err := s.AcceptFriendInvite(ctx, viewer, invite.Code, "Viewer", invite.ExpiresAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired invite: %v", err)
	}
	invite, err = s.CreateFriendInvite(ctx, owner, "Pilot", now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptFriendInvite(ctx, owner, invite.Code, "Pilot", now.Add(24*time.Hour)); !errors.Is(err, ErrInvalidFriend) {
		t.Fatalf("self-accept: %v", err)
	}
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{viewer, other} {
		workers.Go(func() {
			_, err := s.AcceptFriendInvite(ctx, id, " "+invite.Code+" ", "Friend", now.Add(24*time.Hour))
			results <- err
		})
	}
	workers.Wait()
	close(results)
	var accepted, missing int
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrNotFound):
			missing++
		default:
			t.Fatalf("concurrent accept: %v", err)
		}
	}
	if accepted != 1 || missing != 1 {
		t.Fatalf("concurrent acceptance succeeded %d times, missed %d times", accepted, missing)
	}
	if _, err := s.AcceptFriendInvite(ctx, viewer, invite.Code, "Friend", now.Add(24*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("consumed invite: %v", err)
	}
}

func TestFriendAliasValidation(t *testing.T) {
	for _, raw := range []string{"  ", "Alice\nBob", "A\u202eB", "A\u2028B", strings.Repeat("é", 65), string([]byte{0xff})} {
		if _, err := friendAlias(raw); !errors.Is(err, ErrInvalidFriend) {
			t.Errorf("invalid alias %q: %v", raw, err)
		}
	}
	if alias, err := friendAlias("  Ada 👩‍✈️  "); err != nil || alias != "Ada 👩‍✈️" {
		t.Fatalf("valid alias = %q, %v", alias, err)
	}
}
