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

func TestSharedPickupOnlyReturnsTheCurrentProviderTrack(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "pickup-track.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC()
	takeoff := now.Add(-time.Hour)
	flight := feeds.Flight{
		ID: "pickup-track", Number: "XY100", ObservedAt: now, Status: "airborne",
		Source: "flightaware_aeroapi", TailNumber: "N1AA", ActualTakeoff: &takeoff,
	}
	first := feeds.Position{At: takeoff, Latitude: 37, Longitude: -122, Source: flight.Source, AltitudeFeet: new(3400.0)}
	last := feeds.Position{At: now.Add(-time.Minute), Latitude: 42, Longitude: -98, Source: flight.Source, Kind: "ads_b"}
	legacyReceiver := feeds.Position{At: now.Add(-2 * time.Minute), Latitude: 41, Longitude: -101, Source: "operator_readsb"}
	old := feeds.Position{At: takeoff.Add(-time.Second), Latitude: 38, Longitude: -123, Source: flight.Source}
	future := feeds.Position{At: now.Add(time.Minute), Latitude: 43, Longitude: -95, Source: flight.Source}
	invalid := feeds.Position{At: now.Add(-time.Second), Latitude: 91, Longitude: -95, Source: flight.Source}
	if err := s.Observe(ctx, flight, []feeds.Position{last, first, legacyReceiver, old, future, invalid}, nil, nil); err != nil {
		t.Fatal(err)
	}
	owner, other := friendInstallation(t, s), friendInstallation(t, s)
	for _, installation := range []string{owner, other} {
		if err := s.Track(ctx, installation, flight.ID, "30D"); err != nil {
			t.Fatal(err)
		}
	}
	if err := storeRadarSamples(ctx, s, []radarSample{{flight: flight, position: legacyReceiver}}); err != nil {
		t.Fatal(err)
	}
	link, token, err := s.CreatePickupLink(ctx, owner, flight.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got, points, err := s.SharedPickup(ctx, token, now)
	if err != nil || got.ID != flight.ID || len(points) != 2 ||
		points[0] != (feeds.Position{At: first.At, Latitude: first.Latitude, Longitude: first.Longitude}) ||
		points[1] != (feeds.Position{At: last.At, Latitude: last.Latitude, Longitude: last.Longitude}) {
		t.Fatalf("shared pickup = %q, track %+v, error %v", got.ID, points, err)
	}
	for _, lookup := range []struct {
		token string
		at    time.Time
	}{{token, now.Add(time.Hour)}, {"invalid-pickup-token", now}} {
		if got, points, err := s.SharedPickup(ctx, lookup.token, lookup.at); err != ErrNotFound || got.ID != "" || len(points) != 0 {
			t.Fatalf("invalid capability = %q, %d points, %v", got.ID, len(points), err)
		}
	}
	flight.TailNumber = "N2BB"
	flight.ObservedAt = now.Add(time.Second)
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, points, err := s.SharedPickup(ctx, token, flight.ObservedAt); err != nil || got.TailNumber != "N2BB" || len(points) != 0 {
		t.Fatalf("reassigned aircraft = %q, %d points, %v", got.TailNumber, len(points), err)
	}
	if err := s.RevokePickupLink(ctx, owner, link.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SharedPickup(ctx, token, now); err != ErrNotFound {
		t.Fatalf("revoked capability = %v", err)
	}
	_, token, err = s.CreatePickupLink(ctx, owner, flight.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Untrack(ctx, owner, flight.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SharedPickup(ctx, token, now); err != ErrNotFound {
		t.Fatalf("untracked owner's capability = %v", err)
	}
}

func TestPickupTrackKeepsEveryCachedFixForGapDetection(t *testing.T) {
	now := time.Now().UTC()
	flight := feeds.Flight{Source: "plano_demo"}
	positions := make([]feeds.Position, 2000)
	first := now.Add(-time.Hour)
	for i := range positions {
		positions[i] = feeds.Position{At: first.Add(time.Duration(i) * time.Second), Source: flight.Source}
	}
	last := positions[len(positions)-1].At
	points := pickupTrack(flight, positions, now)
	if len(points) != len(positions) || !points[0].At.Equal(first) || !points[len(points)-1].At.Equal(last) {
		t.Fatalf("track lost cached fixes: %d points", len(points))
	}
	for i := 1; i < len(points); i++ {
		if !points[i].At.After(points[i-1].At) {
			t.Fatal("track is out of order")
		}
	}
}
