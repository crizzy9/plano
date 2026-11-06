package core

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestTFMSScopedCancellationRetiresOnlyCoveredFlights(t *testing.T) {
	store, owner, _ := dispatcherFixture(t)
	ctx := t.Context()
	now := time.Now().UTC()
	covered := noticeFlight(now)
	covered.ID, covered.ObservedAt = "covered", now
	outside := covered
	outside.ID, outside.ScheduledArrival = "outside", new(now.Add(3*time.Hour))
	for _, flight := range []feeds.Flight{covered, outside} {
		if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Register(ctx, owner, Device{Platform: "android", Kind: "alert", Token: "tfms-fixture"}, now); err != nil {
		t.Fatal(err)
	}
	claim := func() []deliveryJob {
		t.Helper()
		jobs, err := store.claim(ctx, time.Now(), 10)
		if err != nil {
			t.Fatal(err)
		}
		return slices.DeleteFunc(jobs, func(job deliveryJob) bool { return job.message.AdvisoryWindow == nil })
	}
	program := feeds.Advisory{ID: "tfms/gdp/EWR", Source: feeds.FAASWIM, Airport: "EWR", Kind: "ground_delay", Direction: "arrival",
		AirportContextOnly: true, Reason: "Airport program", ObservedAt: now.Add(-10 * time.Minute),
		StartsAt: new(now.Add(-time.Hour)), EndsAt: new(now.Add(4 * time.Hour))}
	publishDispatchAdvisory(t, store, program, now)
	jobs := claim()
	if len(jobs) != 2 {
		t.Fatalf("initial program claims = %d, want 2", len(jobs))
	}
	withdrawal := feeds.Advisory{Source: program.Source, Kind: feeds.AdvisoryTFMSCancellation, Airport: program.Airport,
		TFMSProgramID: program.ID, Direction: "arrival", AirportContextOnly: true, ObservedAt: now.Add(-5 * time.Minute),
		StartsAt: new(now.Add(time.Hour)), EndsAt: new(now.Add(150 * time.Minute))}
	withdrawal.ID = "tfms/cnx/gdp/EWR/" + withdrawal.ObservedAt.Format("20060102") + "/001"
	publishDispatchAdvisory(t, store, withdrawal, now)
	for _, tc := range []struct {
		flight feeds.Flight
		want   int
	}{{covered, 0}, {outside, 1}} {
		if notices, err := store.FlightNotices(ctx, tc.flight, now); err != nil || len(notices) != tc.want {
			t.Fatalf("%s program notices = %+v, want %d, error = %v", tc.flight.ID, notices, tc.want, err)
		}
	}
	var sent []string
	logger := slog.New(slog.DiscardHandler)
	deliver := pushFunc(func(_ context.Context, _ push.Target, message push.Message) error {
		sent = append(sent, message.FlightID)
		return nil
	})
	for _, job := range jobs {
		store.deliver(ctx, deliver, logger, job)
	}
	if !slices.Equal(sent, []string{outside.ID}) {
		t.Fatalf("leases delivered after cancellation = %v", sent)
	}
	program.ObservedAt, program.Reason = now.Add(-7*time.Minute), "Late earlier revision"
	publishDispatchAdvisory(t, store, program, time.Now())
	if jobs := claim(); len(jobs) != 1 || jobs[0].message.FlightID != outside.ID {
		t.Fatalf("earlier revision queued %d jobs, want just the uncovered flight", len(jobs))
	}
	program.ObservedAt, program.Reason = now.Add(-time.Minute), "Airport program"
	publishDispatchAdvisory(t, store, program, time.Now())
	jobs = claim()
	if len(jobs) != 1 || jobs[0].message.FlightID != covered.ID {
		t.Fatalf("restored unchanged program queued %d jobs, want just the previously covered flight", len(jobs))
	}
	store.deliver(ctx, deliver, logger, jobs[0])
	if !slices.Equal(sent, []string{outside.ID, covered.ID}) {
		t.Fatalf("restored program delivery sequence = %v", sent)
	}
	program.ObservedAt = now.Add(-30 * time.Second)
	publishDispatchAdvisory(t, store, program, time.Now())
	if jobs := claim(); len(jobs) != 0 {
		t.Fatalf("unchanged restored program queued %d extra alerts", len(jobs))
	}
}
