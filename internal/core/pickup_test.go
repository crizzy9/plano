package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestLegacyPickupLinkGetsARevocableHandle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	now := time.Now().UTC()
	flight := feeds.Flight{ID: "old-pickup", Number: "XY100", ObservedAt: now, Status: "scheduled"}
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	oldSecret := randomToken()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO shares VALUES(?,?,?,?)`, hash(oldSecret), owner, flight.ID, now.Add(time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	links, err := s.PickupLinks(ctx, owner, flight.ID, now)
	if err != nil || len(links) != 1 || links[0].ID == oldSecret || links[0].ID == hash(oldSecret) || links[0].CreatedAt != nil {
		t.Fatalf("legacy metadata %#v %v", links, err)
	}
	if err := s.RevokePickupLink(ctx, owner, links[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Shared(ctx, oldSecret, now); err != ErrNotFound {
		t.Fatalf("legacy capability still valid: %v", err)
	}
}
