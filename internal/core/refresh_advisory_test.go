package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

type advisoryRefreshFlights struct {
	feeds.FlightSource
	flight          feeds.Flight
	onGet, onRotate func(context.Context) error
}

func (f advisoryRefreshFlights) Get(ctx context.Context, _ string) (feeds.Flight, error) {
	if f.onGet != nil {
		return f.flight, f.onGet(ctx)
	}
	return f.flight, nil
}

func (f advisoryRefreshFlights) Rotation(ctx context.Context, _ feeds.Flight) ([]feeds.Flight, error) {
	if f.onRotate != nil {
		return nil, f.onRotate(ctx)
	}
	return nil, nil
}

type unnamedAdvisoryPoll struct {
	rows []feeds.Advisory
	err  error
}

func (p unnamedAdvisoryPoll) Advisories(context.Context) ([]feeds.Advisory, error) {
	return p.rows, p.err
}

func newAdvisoryRefresh(t *testing.T, now time.Time, prior []feeds.Advisory) (*Store, Refresher, advisoryRefreshFlights) {
	t.Helper()
	store, err := OpenWithOptions(filepath.Join(t.TempDir(), "advisory-refresh.db"), StoreOptions{CollectDelaySamples: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	flight := feeds.Flight{
		ID: "refresh-advisory", Number: "AA2", Origin: "SFO", OriginICAO: "KSFO", Destination: "JFK",
		Status: "scheduled", TailNumber: "N123AA", ScheduledDeparture: new(now.Add(5 * time.Minute)),
		ObservedAt: now.Add(-time.Minute),
	}
	if err := store.Observe(t.Context(), flight, nil, nil, prior); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(t.Context(), friendInstallation(t, store), flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	flight.ObservedAt = now
	return store, Refresher{Store: store, Logger: slog.New(slog.DiscardHandler)}, advisoryRefreshFlights{flight: flight}
}

func refreshDelayAdvisory(id, airport string, observed time.Time, delay int) feeds.Advisory {
	return feeds.Advisory{ID: id, Source: feeds.FAASWIM, Airport: airport, Direction: "departure",
		Kind: "departure_delay", Reason: "ATC staffing", ObservedAt: observed,
		EndsAt: new(observed.Add(time.Hour)), DelayMinutes: new(delay)}
}

func TestPeriodicForecastReconcilesSignedRevisionDuringFlightFetch(t *testing.T) {
	for _, tc := range []struct {
		name, airport string
		delay, want   int
		expired       bool
	}{
		{"replacement", "KSFO", 60, 60, false},
		{"moved and expired revision", "KSEA", 60, 15, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC().Add(-2 * time.Second)
			old := refreshDelayAdvisory("updated", "KSFO", now.Add(-2*time.Minute), 30)
			independent := refreshDelayAdvisory("independent", "KSFO", old.ObservedAt, 15)
			store, refresher, flights := newAdvisoryRefresh(t, now, []feeds.Advisory{old})
			if err := store.PublishAdvisory(t.Context(), old, now); err != nil {
				t.Fatal(err)
			}
			flights.onGet = func(ctx context.Context) error {
				at := time.Now().UTC()
				updated := refreshDelayAdvisory(old.ID, tc.airport, at, tc.delay)
				if tc.expired {
					updated.EndsAt = new(at.Add(-time.Second))
				}
				return store.PublishAdvisory(ctx, updated, at)
			}
			refresher.Flights = flights
			refresher.Advisories = []feeds.AdvisorySource{unnamedAdvisoryPoll{rows: []feeds.Advisory{old, independent}}}
			if err := refresher.refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, err := store.Details(t.Context(), flights.flight.ID)
			if err != nil || got.Prediction == nil || got.Prediction.DelayMinutes != tc.want || !got.Flight.ObservedAt.Equal(now) {
				t.Fatalf("forecast after signed revision = %+v, flight clock = %s, err = %v", got.Prediction, got.Flight.ObservedAt, err)
			}
		})
	}
}

func TestPeriodicForecastAndTrainingUseAdvisoryArrivingDuringRotation(t *testing.T) {
	now := time.Now().UTC().Add(-2 * time.Second)
	store, refresher, flights := newAdvisoryRefresh(t, now, nil)
	var received time.Time
	flights.onRotate = func(ctx context.Context) error {
		received = time.Now().UTC()
		return store.PublishAdvisory(ctx, refreshDelayAdvisory("new", "KSFO", received, 42), received)
	}
	refresher.Flights = flights
	if err := refresher.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := store.Details(t.Context(), flights.flight.ID)
	if err != nil || got.Prediction == nil || got.Prediction.DelayMinutes != 42 || !got.Flight.ObservedAt.Equal(now) {
		t.Fatalf("forecast after rotation = %+v, flight clock = %s, err = %v", got.Prediction, got.Flight.ObservedAt, err)
	}
	var observed int64
	var raw []byte
	if err := store.db.QueryRowContext(t.Context(), `SELECT observed_ns,features FROM delay_samples WHERE flight_id=?`, flights.flight.ID).Scan(&observed, &raw); err != nil {
		t.Fatal(err)
	}
	var features predict.Features
	if err := json.Unmarshal(raw, &features); err != nil || features.AirportDelayMinutes != 42 || observed < received.UnixNano() {
		t.Fatalf("training sample = %+v at %s, advisory received = %s, err = %v", features, time.Unix(0, observed), received, err)
	}
}

func TestPeriodicForecastHonorsNewerPolledRevisionOutsideRoute(t *testing.T) {
	now := time.Now().UTC().Add(-2 * time.Second)
	signed := refreshDelayAdvisory("moved", "KSFO", now.Add(-2*time.Minute), 60)
	store, refresher, flights := newAdvisoryRefresh(t, now, nil)
	if err := store.PublishAdvisory(t.Context(), signed, now); err != nil {
		t.Fatal(err)
	}
	moved := refreshDelayAdvisory(signed.ID, "KSEA", now.Add(-time.Minute), 60)
	local := refreshDelayAdvisory("local", "KSFO", moved.ObservedAt, 15)
	refresher.Flights = flights
	refresher.Advisories = []feeds.AdvisorySource{unnamedAdvisoryPoll{rows: []feeds.Advisory{moved, local}}}
	if err := refresher.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := store.Details(t.Context(), flights.flight.ID)
	if err != nil || got.Prediction == nil || got.Prediction.DelayMinutes != 15 {
		t.Fatalf("the newer out-of-route poll should retire the older signed delay: %+v, %v", got.Prediction, err)
	}
}

func TestPeriodicForecastDistinguishesAdvisoryOutageFromEmptyPoll(t *testing.T) {
	now := time.Now().UTC().Add(-2 * time.Second)
	old := refreshDelayAdvisory("unnamed", "KSFO", now.Add(-2*time.Minute), 30)
	store, refresher, flights := newAdvisoryRefresh(t, now, []feeds.Advisory{old})
	refresher.Flights = flights
	refresher.Advisories = []feeds.AdvisorySource{unnamedAdvisoryPoll{err: feeds.ErrUnavailable}}
	if err := refresher.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if detail, err := store.Details(t.Context(), flights.flight.ID); err != nil || detail.Prediction == nil || detail.Prediction.DelayMinutes != 30 {
		t.Fatalf("outage did not preserve the prior valid forecast: %+v, %v", detail.Prediction, err)
	}
	flights.flight.ObservedAt = now.Add(time.Second)
	refresher.Flights = flights
	refresher.Advisories = []feeds.AdvisorySource{unnamedAdvisoryPoll{}}
	if err := refresher.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if detail, err := store.Details(t.Context(), flights.flight.ID); err != nil || detail.Prediction != nil {
		t.Fatalf("successful empty poll did not clear the forecast: %+v, %v", detail.Prediction, err)
	}
}
