package core

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestAeroHintPersistsAndRequeuesNewGeneration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "aero.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	now := time.Now().UTC()
	f := feeds.Flight{ID: "AAL33-1", Source: "flightaware_aeroapi", ObservedAt: now, Status: "scheduled"}
	if err := s.Observe(ctx, f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAeroRefresh(ctx, f.ID, now); err != nil {
		t.Fatal(err)
	}
	if job, err := s.claimAeroRefresh(ctx, now); err != nil || job != nil {
		t.Fatalf("untracked job %v %v", job, err)
	}
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, f.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAeroRefresh(ctx, f.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.claimAeroRefresh(ctx, now)
	if err != nil || old == nil {
		t.Fatalf("lost persisted job %v %v", old, err)
	}
	later := now.Add(time.Second)
	if err := s.QueueAeroRefresh(ctx, f.ID, later); err != nil {
		t.Fatal(err)
	}
	if err := s.completeAeroRefresh(ctx, old, later, false); err != nil {
		t.Fatal(err)
	}
	newJob, err := s.claimAeroRefresh(ctx, later)
	if err != nil || newJob == nil || newJob.generation != old.generation+1 {
		t.Fatalf("lost concurrent hint %v %v", newJob, err)
	}
	if err := s.completeAeroRefresh(ctx, newJob, later, true); err != nil {
		t.Fatal(err)
	}
	if job, err := s.claimAeroRefresh(ctx, later); err != nil || job != nil {
		t.Fatalf("backoff job %v %v", job, err)
	}
}

func TestAeroHintsPreserveRefreshRetryBackoff(t *testing.T) {
	ctx := t.Context()
	s, id, now := queuedAeroRefreshFixture(t)
	first, err := s.claimAeroRefresh(ctx, now)
	if err != nil || first == nil {
		t.Fatalf("initial claim = %+v, %v", first, err)
	}
	failedAt := now.Add(time.Second)
	if err := s.QueueAeroRefresh(ctx, id, failedAt.Add(-time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := s.completeAeroRefresh(ctx, first, failedAt, true); err != nil {
		t.Fatal(err)
	}
	firstRetry := failedAt.Add(time.Second)
	if early, err := s.claimAeroRefresh(ctx, firstRetry.Add(-time.Nanosecond)); err != nil || early != nil {
		t.Fatalf("hint during failed lease shortened backoff: %+v, %v", early, err)
	}
	retry, err := s.claimAeroRefresh(ctx, firstRetry)
	if err != nil || retry == nil || retry.generation != first.generation+1 || retry.failures != 1 {
		t.Fatalf("first retry = %+v, %v", retry, err)
	}
	if err := s.completeAeroRefresh(ctx, retry, firstRetry, true); err != nil {
		t.Fatal(err)
	}
	secondRetry := firstRetry.Add(2 * time.Second)
	if err := s.QueueAeroRefresh(ctx, id, secondRetry.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if early, err := s.claimAeroRefresh(ctx, secondRetry.Add(-time.Nanosecond)); err != nil || early != nil {
		t.Fatalf("hint during retry wait shortened backoff: %+v, %v", early, err)
	}
	success, err := s.claimAeroRefresh(ctx, secondRetry)
	if err != nil || success == nil || success.generation != first.generation+2 || success.failures != 2 {
		t.Fatalf("second retry = %+v, %v", success, err)
	}
	finished := secondRetry.Add(time.Second)
	if err := s.QueueAeroRefresh(ctx, id, finished.Add(-time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := s.completeAeroRefresh(ctx, success, finished, false); err != nil {
		t.Fatal(err)
	}
	latest, err := s.claimAeroRefresh(ctx, finished)
	if err != nil || latest == nil || latest.generation != first.generation+3 || latest.failures != 0 {
		t.Fatalf("immediate refresh after success = %+v, %v", latest, err)
	}
}

func TestAeroRefreshRetryLimitKeepsOnlyNewerHints(t *testing.T) {
	ctx := t.Context()
	s, id, due := queuedAeroRefreshFixture(t)
	for failures := range 7 {
		job, err := s.claimAeroRefresh(ctx, due)
		if err != nil || job == nil || job.failures != failures || job.generation != 1 {
			t.Fatalf("claim after %d failures = %+v, %v", failures, job, err)
		}
		if failures == 6 {
			if err := s.QueueAeroRefresh(ctx, id, due); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.completeAeroRefresh(ctx, job, due, true); err != nil {
			t.Fatal(err)
		}
		due = due.Add(time.Second << failures)
		if early, err := s.claimAeroRefresh(ctx, due.Add(-time.Nanosecond)); err != nil || early != nil {
			t.Fatalf("early claim after %d failures = %+v, %v", failures+1, early, err)
		}
	}
	if err := s.QueueAeroRefresh(ctx, id, due.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if early, err := s.claimAeroRefresh(ctx, due.Add(-time.Nanosecond)); err != nil || early != nil {
		t.Fatalf("hint shortened capped delay: %+v, %v", early, err)
	}
	latest, err := s.claimAeroRefresh(ctx, due)
	if err != nil || latest == nil || latest.failures != 6 || latest.generation != 3 {
		t.Fatalf("claim for newer hints after retry limit = %+v, %v", latest, err)
	}
	if err := s.completeAeroRefresh(ctx, latest, due, true); err != nil {
		t.Fatal(err)
	}
	if job, err := s.claimAeroRefresh(ctx, due.Add(time.Hour)); err != nil || job != nil {
		t.Fatalf("exhausted hint was retried without a new hint: %+v, %v", job, err)
	}
}

func queuedAeroRefreshFixture(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "aero-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC()
	flight := feeds.Flight{ID: "AAL33-retry", Source: "flightaware_aeroapi", ObservedAt: now, Status: "scheduled"}
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	trackAeroCallbackFixture(t, s, flight.ID)
	if err := s.QueueAeroRefresh(ctx, flight.ID, now); err != nil {
		t.Fatal(err)
	}
	return s, flight.ID, now
}

func TestAeroInboundHintRequiresEligibleDirectLink(t *testing.T) {
	const providerID = "AAL32-1"
	now := time.Now().UTC()
	for _, tc := range []struct {
		name    string
		untrack bool
		change  func(*feeds.Flight, *[]feeds.Flight)
		want    bool
	}{
		{name: "explicit link before rotation is cached", want: true},
		{name: "matching first leg", change: func(_ *feeds.Flight, rotation *[]feeds.Flight) {
			*rotation = []feeds.Flight{{ID: providerID}}
		}, want: true},
		{name: "matching older leg only", change: func(_ *feeds.Flight, rotation *[]feeds.Flight) {
			*rotation = []feeds.Flight{{ID: "AAL31-1"}, {ID: providerID}}
		}},
		{name: "inferred link only", change: func(onward *feeds.Flight, rotation *[]feeds.Flight) {
			onward.InboundID = ""
			*rotation = []feeds.Flight{{ID: providerID}}
		}},
		{name: "unknown instance", change: func(onward *feeds.Flight, _ *[]feeds.Flight) {
			onward.InboundID = "AAL31-1"
		}},
		{name: "untracked onward", untrack: true},
		{name: "non-AeroAPI onward", change: func(onward *feeds.Flight, _ *[]feeds.Flight) {
			onward.Source = "demo"
		}},
		{name: "old onward", change: func(onward *feeds.Flight, _ *[]feeds.Flight) {
			onward.ScheduledDeparture = new(now.Add(-7 * time.Hour))
		}},
		{name: "far future onward", change: func(onward *feeds.Flight, _ *[]feeds.Flight) {
			onward.ScheduledDeparture = new(now.Add(26 * time.Hour))
		}},
		{name: "already left the gate", change: func(onward *feeds.Flight, _ *[]feeds.Flight) {
			onward.ActualDeparture = new(now.Add(-time.Minute))
		}},
		{name: "cancelled onward", change: func(onward *feeds.Flight, _ *[]feeds.Flight) {
			onward.Status = "cancelled"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s, err := Open(filepath.Join(t.TempDir(), "inbound-hint.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			onward := feeds.Flight{ID: "AAL33-1", Source: "flightaware_aeroapi", Status: "scheduled", InboundID: providerID,
				ScheduledDeparture: new(now.Add(time.Hour)), ObservedAt: now}
			var rotation []feeds.Flight
			if tc.change != nil {
				tc.change(&onward, &rotation)
			}
			if err := s.Observe(ctx, onward, nil, rotation, nil); err != nil {
				t.Fatal(err)
			}
			if !tc.untrack {
				trackAeroCallbackFixture(t, s, onward.ID)
			}
			if err := s.QueueAeroRefresh(ctx, providerID, now); err != nil {
				t.Fatal(err)
			}
			job, err := s.claimAeroRefresh(ctx, now)
			if err != nil || (job != nil) != tc.want || job != nil && job.providerID != providerID {
				t.Fatalf("inbound job = %+v, want queued %v: %v", job, tc.want, err)
			}
			if !tc.want {
				source := new(countingAeroSource)
				if err := s.refreshAeroFlight(ctx, source, providerID); err != nil || len(source.ids) != 0 {
					t.Fatalf("ineligible inbound fetches = %v: %v", source.ids, err)
				}
			}
		})
	}
}

func TestAeroCallbackSharesOneFetchWithDirectAliasAndOnward(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "inbound-fetch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Add(-time.Minute)
	inbound := feeds.Flight{ID: "AAL32-1", Number: "AA32", Source: "flightaware_aeroapi", Status: "airborne",
		Origin: "ORD", Destination: "JFK", TailNumber: "N123AA", ObservedAt: now.Add(-time.Minute),
		ScheduledDeparture: new(now.Add(-3 * time.Hour)), ActualTakeoff: new(now.Add(-2 * time.Hour)),
		ScheduledArrival: new(now.Add(15 * time.Minute)), EstimatedArrival: new(now.Add(15 * time.Minute))}
	onward := feeds.Flight{ID: "AAL33-1", Number: "AA33", Source: "flightaware_aeroapi", Status: "scheduled", InboundID: inbound.ID,
		Origin: "JFK", Destination: "BOS", TailNumber: inbound.TailNumber, ObservedAt: inbound.ObservedAt,
		ScheduledDeparture: new(now.Add(time.Hour))}
	if err := s.Observe(ctx, onward, nil, []feeds.Flight{inbound}, nil); err != nil {
		t.Fatal(err)
	}
	trackAeroCallbackFixture(t, s, onward.ID)
	local := inbound
	local.ID, local.Source = "local-inbound-alias", "flightaware_schedule"
	if err := s.Observe(ctx, local, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	trackAeroCallbackFixture(t, s, local.ID)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO aero_instance_keys VALUES(?,?)`, inbound.ID, local.ID); err != nil {
		t.Fatal(err)
	}
	freshArrival := now.Add(35 * time.Minute)
	body, err := json.Marshal(map[string]any{"flights": []map[string]any{{
		"fa_flight_id": inbound.ID, "ident_iata": inbound.Number, "registration": inbound.TailNumber,
		"origin": map[string]string{"code_iata": inbound.Origin}, "destination": map[string]string{"code_iata": inbound.Destination},
		"scheduled_out": inbound.ScheduledDeparture, "actual_off": inbound.ActualTakeoff,
		"scheduled_in": inbound.ScheduledArrival, "estimated_in": freshArrival,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	source := feeds.NewAeroAPI("test-key", &http.Client{Transport: aeroCallbackTransport(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})})
	if err := s.refreshAeroFlight(ctx, source, inbound.ID); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/aeroapi/flights/"+inbound.ID {
		t.Fatalf("expected one provider fetch, got %v", paths)
	}
	direct, err := s.Flight(ctx, local.ID)
	if err != nil || !direct.ObservedAt.After(inbound.ObservedAt) || direct.EstimatedArrival == nil || !direct.EstimatedArrival.Equal(freshArrival) {
		t.Fatalf("direct alias did not receive the refresh: %+v, %v", direct, err)
	}
	var alias string
	if err := s.db.QueryRowContext(ctx, `SELECT provider_id FROM aero_instance_keys WHERE flight_id=?`, local.ID).Scan(&alias); err != nil || alias != inbound.ID {
		t.Fatalf("direct alias changed to %q: %v", alias, err)
	}
	details, err := s.Details(ctx, onward.ID)
	if err != nil || !details.Flight.ObservedAt.Equal(onward.ObservedAt) || len(details.Inbound) != 1 ||
		details.Inbound[0].ID != inbound.ID || !details.Inbound[0].ObservedAt.Equal(direct.ObservedAt) {
		t.Fatalf("onward did not receive the same provider revision: %+v, %v", details, err)
	}
}

func trackAeroCallbackFixture(t *testing.T, s *Store, flightID string) {
	t.Helper()
	ctx := t.Context()
	token, err := s.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Installation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Track(ctx, owner, flightID, ""); err != nil {
		t.Fatal(err)
	}
}

type countingAeroSource struct {
	feeds.FlightSource
	ids []string
}

func (source *countingAeroSource) Get(_ context.Context, id string) (feeds.Flight, error) {
	source.ids = append(source.ids, id)
	return feeds.Flight{}, feeds.ErrUnavailable
}

type aeroCallbackTransport func(*http.Request) (*http.Response, error)

func (transport aeroCallbackTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

type gatedAeroSource struct {
	feeds.FlightSource
	started, release chan struct{}
	flight           feeds.Flight
}

func (source gatedAeroSource) Get(ctx context.Context, _ string) (feeds.Flight, error) {
	close(source.started)
	select {
	case <-source.release:
		return source.flight, nil
	case <-ctx.Done():
		return feeds.Flight{}, ctx.Err()
	}
}

func TestInFlightCallbackDoesNotRevertRemappedAlias(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := Open(filepath.Join(t.TempDir(), "alias.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	local := feeds.Flight{ID: "local-instance", Number: "AA2", Origin: "LAX", Destination: "JFK", Status: "scheduled", ObservedAt: now.Add(-time.Minute), DepartureGate: "B2"}
	if err := s.Observe(ctx, local, nil, nil, nil); err != nil {
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
	if err := s.Track(ctx, owner, local.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO aero_instance_keys VALUES(?,?)`, "AAL2-1", local.ID); err != nil {
		t.Fatal(err)
	}
	remote := local
	remote.ID, remote.Source, remote.ObservedAt, remote.DepartureGate = "AAL2-1", "flightaware_aeroapi", now, "C1"
	source := gatedAeroSource{started: make(chan struct{}), release: make(chan struct{}), flight: remote}
	done := make(chan error, 1)
	go func() { done <- s.refreshAeroFlight(ctx, source, remote.ID) }()
	select {
	case <-source.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE aero_instance_keys SET provider_id=? WHERE flight_id=?`, "AAL2-2", local.ID); err != nil {
		t.Fatal(err)
	}
	close(source.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var key string
	if err := s.db.QueryRowContext(ctx, `SELECT provider_id FROM aero_instance_keys WHERE flight_id=?`, local.ID).Scan(&key); err != nil || key != "AAL2-2" {
		t.Fatalf("alias reverted: %s %v", key, err)
	}
	flight, err := s.Flight(ctx, local.ID)
	if err != nil || flight.DepartureGate != local.DepartureGate {
		t.Fatalf("snapshot overwritten: %#v %v", flight, err)
	}
}
