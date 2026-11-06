package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func noticeFlight(now time.Time) feeds.Flight {
	return feeds.Flight{ID: "notice-flight", Status: "scheduled", Origin: "SFO", OriginICAO: "KSFO",
		Destination: "EWR", DestinationICAO: "KEWR", ScheduledDeparture: new(now.Add(10 * time.Minute)),
		ScheduledArrival: new(now.Add(2 * time.Hour))}
}

func TestFlightNoticesMatchPublishedOperationAndWindow(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "notices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	flight := noticeFlight(now)
	common := feeds.Advisory{Source: feeds.FAASWIM, ObservedAt: now.Add(-time.Minute), Kind: "runway_closure",
		Airport: "KEWR", Direction: "arrival", EndsAt: new(now.Add(3 * time.Hour))}
	matching := common
	matching.ID, matching.RawReason = "runway", "!EWR EWR RWY 04/22 CLSD"
	departing := common
	departing.ID, departing.Airport, departing.Direction, departing.Reason = "origin", "SFO", "departure", "Taxiway work\n\u202e and signage"
	groundStop := common
	groundStop.ID, groundStop.Kind, groundStop.EndsAt = "ground-stop", "ground_stop", new(now.Add(20*time.Minute))
	unrelated := common
	unrelated.ID, unrelated.Airport = "other", "KEWRX"
	wrongDirection := common
	wrongDirection.ID, wrongDirection.Direction = "wrong-direction", "departure"
	lateStart := common
	lateStart.ID, lateStart.StartsAt = "late-start", new(now.Add(150*time.Minute))
	endedAtOperation := common
	endedAtOperation.ID, endedAtOperation.EndsAt = "end-exclusive", flight.ScheduledArrival
	cancelled := common
	cancelled.ID, cancelled.RawReason = "cancelled", "A0124/26 NOTAMC A0123/26"
	futureObserved := common
	futureObserved.ID, futureObserved.ObservedAt = "future", now.Add(time.Minute)
	wrongRawAirport := common
	wrongRawAirport.ID, wrongRawAirport.RawReason = "wrong-raw-airport", "!JFK JFK RWY 04/22 CLSD"
	if err := s.cachePolledFlightNotices(ctx, common.Source, []feeds.Advisory{
		matching, departing, groundStop, unrelated, wrongDirection, lateStart, endedAtOperation, cancelled, futureObserved, wrongRawAirport,
	}, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.FlightNotices(ctx, flight, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Kind != "ground_stop" || got[0].Operation != "arrival" ||
		got[1].Summary != "Taxiway work and signage" || got[1].Operation != "departure" || got[1].Airport != "SFO" ||
		got[2].Summary != "Runway 04/22 closed" || got[2].Airport != "KEWR" || got[2].Source != feeds.FAASWIM {
		t.Fatalf("unexpected route notices: %+v", got)
	}
	if !got[0].ValidUntil.Equal(*groundStop.EndsAt) || !got[2].ValidUntil.Equal(now.Add(polledNoticeMaxAge)) {
		t.Fatalf("wrong cache validity: %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), "rawReason") || strings.Contains(string(encoded), "CLSD") {
		t.Fatalf("raw notice leaked: %s, %v", encoded, err)
	}
	flight.Status, flight.ActualDeparture = "airborne", new(now)
	got, err = s.FlightNotices(ctx, flight, now)
	if err != nil || len(got) != 1 || got[0].Summary != "Runway 04/22 closed" {
		t.Fatalf("airborne context = %+v, %v", got, err)
	}
	flight.Status = "diverted"
	got, err = s.FlightNotices(ctx, flight, now)
	if err != nil || len(got) != 0 {
		t.Fatalf("diverted context = %+v, %v", got, err)
	}
}

func TestFlightNoticesPreferSignedRevisionAndKeepEndedTombstone(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "notices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	flight := noticeFlight(now)
	polled := feeds.Advisory{ID: "shared", Source: feeds.FAASWIM, Kind: "runway_closure", Airport: "SFO",
		Direction: "departure", ObservedAt: now.Add(-2 * time.Minute), EndsAt: new(now.Add(time.Hour)), Reason: "Poll runway notice"}
	other := polled
	other.Source, other.Reason = feeds.Eurocontrol, "Separate source notice"
	if err := s.cachePolledFlightNotices(ctx, polled.Source, []feeds.Advisory{polled}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.cachePolledFlightNotices(ctx, other.Source, []feeds.Advisory{other}, now); err != nil {
		t.Fatal(err)
	}
	signed := polled
	signed.ObservedAt, signed.Reason = now.Add(-time.Minute), "Signed runway notice"
	if err := s.PublishAdvisory(ctx, signed, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.FlightNotices(ctx, flight, now)
	if err != nil || len(got) != 2 || got[0].Summary != signed.Reason || got[1].Summary != other.Reason {
		t.Fatalf("signed revision or source isolation failed: %+v, %v", got, err)
	}
	ended := signed
	ended.ObservedAt, ended.EndsAt = now, new(now)
	if err := s.PublishAdvisory(ctx, ended, now); err != nil {
		t.Fatal(err)
	}
	if err := s.cachePolledFlightNotices(ctx, polled.Source, []feeds.Advisory{polled}, now); err != nil {
		t.Fatal(err)
	}
	got, err = s.FlightNotices(ctx, flight, now)
	if err != nil || len(got) != 1 || got[0].Summary != other.Reason {
		t.Fatalf("ended revision let an old poll reappear: %+v, %v", got, err)
	}
}

func TestFlightNoticePollCachePersistsExpiresAndClears(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "notices.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	flight := noticeFlight(now)
	newer := feeds.Advisory{ID: "shared", Source: "faa_nas", Kind: "airport_restriction", Airport: "SFO",
		ObservedAt: now.Add(-time.Minute), EndsAt: new(now.Add(time.Hour)), Reason: "Current notice"}
	older := newer
	older.ObservedAt, older.Reason = now.Add(-2*time.Minute), "Older notice"
	if err := s.cachePolledFlightNotices(ctx, newer.Source, []feeds.Advisory{newer}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.cachePolledFlightNotices(ctx, older.Source, []feeds.Advisory{older}, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.FlightNotices(ctx, flight, now.Add(25*time.Minute))
	if err != nil || len(got) != 1 || got[0].Summary != newer.Reason || !got[0].ValidUntil.Equal(now.Add(polledNoticeMaxAge)) {
		t.Fatalf("last successful source state not preserved: %+v, %v", got, err)
	}
	got, err = s.FlightNotices(ctx, flight, now.Add(polledNoticeMaxAge))
	if err != nil || len(got) != 0 {
		t.Fatalf("stale poll still visible: %+v, %v", got, err)
	}
	if err := s.cachePolledFlightNotices(ctx, newer.Source, nil, now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err = s.FlightNotices(ctx, flight, now.Add(25*time.Minute))
	if err != nil || len(got) != 0 {
		t.Fatalf("successful empty snapshot did not clear the source: %+v, %v", got, err)
	}
}

func TestFlightNoticeResponseBoundKeepsNewest(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "notices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	var polled []feeds.Advisory
	for i := range 10 {
		polled = append(polled, feeds.Advisory{ID: fmt.Sprint(i), Airport: "SFO", Source: "faa_nas",
			Kind: "departure_delay", ObservedAt: now.Add(-time.Duration(i) * time.Minute), Reason: fmt.Sprint(i)})
	}
	if err := s.cachePolledFlightNotices(ctx, "faa_nas", polled, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.FlightNotices(ctx, noticeFlight(now), now)
	if err != nil || len(got) != maxFlightNotices || got[0].Summary != "0" || got[maxFlightNotices-1].Summary != "7" {
		t.Fatalf("notice response cap or order failed: %+v, %v", got, err)
	}
}

func TestFlightNoticeSummaryHidesUnsupportedWireText(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"!SFO SFO OBST TOWER", "airport restriction"},
		{"(A0012/26 NOTAMN\nA) KSFO\nE) OBST TOWER)", "airport restriction"},
		{"Weather and airport volume", "Weather and airport volume"},
	} {
		got, _, ok := flightNoticeSummary(feeds.Advisory{Kind: "airport_restriction", Reason: tc.raw, RawReason: tc.raw})
		if !ok || got != tc.want {
			t.Errorf("summary for %q = %q, %v, want %q", tc.raw, got, ok, tc.want)
		}
	}
}

type noticePollSource struct {
	rows []feeds.Advisory
	err  error
}

func (*noticePollSource) AdvisorySourceID() string { return "faa_nas" }
func (p *noticePollSource) Advisories(context.Context) ([]feeds.Advisory, error) {
	return p.rows, p.err
}

type noticePollFlights struct {
	feeds.FlightSource
}

func (noticePollFlights) Get(context.Context, string) (feeds.Flight, error) {
	return feeds.Flight{}, feeds.ErrUnavailable
}

func TestFlightNoticeRefreshRetainsLastSuccessOnSourceFailure(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "notices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	now := time.Now().UTC()
	flight := noticeFlight(now)
	flight.ObservedAt = now
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
	feed := &noticePollSource{rows: []feeds.Advisory{{ID: "departures", Airport: "SFO", Source: "faa_nas",
		Kind: "departure_delay", ObservedAt: now, Reason: "Departure restriction"}}}
	refresher := Refresher{Store: s, Flights: noticePollFlights{}, Advisories: []feeds.AdvisorySource{feed}, Logger: slog.New(slog.DiscardHandler)}
	checkRefresh := func(want int) {
		t.Helper()
		if err := refresher.refresh(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := s.FlightNotices(ctx, flight, time.Now())
		if err != nil || len(got) != want {
			t.Fatalf("expected %d cached notices, got %+v, %v", want, got, err)
		}
	}
	checkRefresh(1)
	feed.rows, feed.err = nil, errors.New("feed offline")
	checkRefresh(1)
	feed.err = nil
	checkRefresh(0)
}
