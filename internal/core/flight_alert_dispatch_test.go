package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestFlightAlertDispatcherKeepsCurrentGateAndDepartureMilestones(t *testing.T) {
	store, flight := flightAlertDispatchFixture(t)
	flight.Status, flight.DepartureGate = "boarding", "B2"
	observeFlightAlertFixture(t, store, &flight)
	var boarding, oldGate []deliveryJob
	for _, job := range claimFlightAlertJobs(t, store, 4) {
		switch job.message.Title {
		case "Boarding":
			if got := job.message.ExpiresAt.Sub(job.message.Timestamp); got != boardingAlertLifetime {
				t.Fatalf("boarding lifetime = %v, want %v", got, boardingAlertLifetime)
			}
			boarding = append(boarding, job)
		case "Departure gate":
			oldGate = append(oldGate, job)
		default:
			t.Fatalf("unexpected claimed alert %q", job.message.Title)
		}
	}
	if len(boarding) != 2 || len(oldGate) != 2 {
		t.Fatalf("claimed boarding alerts = %d, gate alerts = %d", len(boarding), len(oldGate))
	}

	flight.DepartureGate = "C3"
	observeFlightAlertFixture(t, store, &flight)
	assertFlightAlertSends(t, store, oldGate)
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2), "Departure gate changed|AA2 · C3")

	flight.Status, flight.ActualDeparture = "taxi_out", new(flight.ObservedAt.Add(time.Millisecond))
	observeFlightAlertFixture(t, store, &flight)
	flight.Status, flight.ActualTakeoff = "airborne", new(flight.ObservedAt.Add(time.Millisecond))
	observeFlightAlertFixture(t, store, &flight)
	assertFlightAlertSends(t, store, boarding)
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 4), "Pushed back from gate|AA2", "Taken off|AA2")
	assertFlightAlertHistory(t, store, flight.ID, "boarding|AA2", "departure_gate|AA2 · B2", "departure_gate|AA2 · C3",
		"taxi_out|AA2", "airborne|AA2")
}

func TestFlightAlertDispatcherKeepsCurrentCarouselAfterGateIn(t *testing.T) {
	store, flight := flightAlertDispatchFixture(t)
	flight.Status, flight.ActualArrival, flight.Baggage = "arrived", new(flight.ObservedAt.Add(time.Millisecond)), "3"
	observeFlightAlertFixture(t, store, &flight)
	flight.Baggage = "7"
	observeFlightAlertFixture(t, store, &flight)
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 4), "Arrived at the gate|AA2", "Baggage carousel changed|AA2 · 7")

	flight.Baggage = "9"
	observeFlightAlertFixture(t, store, &flight)
	flight.Baggage = ""
	observeFlightAlertFixture(t, store, &flight)
	claimFlightAlertJobs(t, store, 0)
	assertFlightAlertHistory(t, store, flight.ID, "arrived|AA2", "baggage|AA2 · 3", "baggage|AA2 · 7", "baggage|AA2 · 9")
}

func TestFlightAlertDispatcherRejectsLegacyBoardingAfterPushback(t *testing.T) {
	store, flight := flightAlertDispatchFixture(t)
	flight.Status = "boarding"
	observeFlightAlertFixture(t, store, &flight)

	// The older writer committed this snapshot without retiring its queued
	// boarding messages. The transport boundary must still reject them.
	flight.ObservedAt = flight.ObservedAt.Add(time.Millisecond)
	flight.Status, flight.ActualDeparture = "taxi_out", new(flight.ObservedAt)
	snapshot, err := json.Marshal(flight)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `UPDATE flights SET snapshot=?, observed_ns=? WHERE id=?`,
		string(snapshot), flight.ObservedAt.UnixNano(), flight.ID); err != nil {
		t.Fatal(err)
	}
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2))
	var remaining int
	err = store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM outbox WHERE flight_id=?`, flight.ID).Scan(&remaining)
	if err != nil || remaining != 0 {
		t.Fatalf("obsolete outbox jobs = %d, error = %v", remaining, err)
	}
	assertFlightAlertHistory(t, store, flight.ID, "boarding|AA2")
}

func TestFlightAlertDispatcherDropsPredictionAfterInboundChange(t *testing.T) {
	store, flight := flightAlertDispatchFixture(t)
	flight.InboundID = "direct-inbound"
	flight.ScheduledDeparture = new(flight.ObservedAt.Add(30 * time.Hour))
	inbound := feeds.Flight{ID: flight.InboundID, Number: "AA1", Destination: flight.Origin, Status: "airborne",
		ObservedAt: flight.ObservedAt, EstimatedArrival: new(flight.ScheduledDeparture.Add(10 * time.Minute))}
	flight.ObservedAt = flight.ObservedAt.Add(time.Millisecond)
	if err := store.observeTracked(t.Context(), flight, nil, []feeds.Flight{inbound}, nil); err != nil {
		t.Fatal(err)
	}
	jobs := claimFlightAlertJobs(t, store, 2)
	flight.InboundID = ""
	observeFlightAlertFixture(t, store, &flight)
	assertFlightAlertSends(t, store, jobs)
	claimFlightAlertJobs(t, store, 0)
	assertFlightAlertHistory(t, store, flight.ID, "delay_prediction|AA2 · 50 min later")
}

func TestFlightAlertDispatcherReoffersUnclaimedCheckInAfterReinstatement(t *testing.T) {
	store, flight := flightAlertDispatchFixture(t)
	flight.ScheduledDeparture = new(flight.ObservedAt.Add(time.Hour))
	observeFlightAlertFixture(t, store, &flight)
	flight.Status = "cancelled"
	observeFlightAlertFixture(t, store, &flight)
	flight.Status = "scheduled"
	observeFlightAlertFixture(t, store, &flight)
	originalDeparture := flight.ScheduledDeparture
	flight.ScheduledDeparture = new(flight.ObservedAt.Add(30 * time.Hour))
	observeFlightAlertFixture(t, store, &flight)
	claimFlightAlertJobs(t, store, 0)
	flight.ScheduledDeparture = originalDeparture
	observeFlightAlertFixture(t, store, &flight)
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2), "Check-in reminder|Check check-in availability for AA2 with your airline.")
	assertFlightAlertHistory(t, store, flight.ID, "cancelled|AA2", "check_in|Check check-in availability for AA2 with your airline.")
}

func flightAlertDispatchFixture(t *testing.T) (*Store, feeds.Flight) {
	t.Helper()
	store, owner, id := dispatcherFixture(t)
	ctx := t.Context()
	flight, err := store.Flight(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, id, ""); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"ios", "android"} {
		device := Device{Platform: platform, Kind: "alert", Token: platform + "-fixture-device"}
		if err := store.Register(ctx, owner, device, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return store, flight
}

func observeFlightAlertFixture(t *testing.T, store *Store, flight *feeds.Flight) {
	t.Helper()
	flight.ObservedAt = flight.ObservedAt.Add(time.Millisecond)
	if err := store.Observe(t.Context(), *flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func claimFlightAlertJobs(t *testing.T, store *Store, want int) []deliveryJob {
	t.Helper()
	jobs, err := store.claim(t.Context(), time.Now().Add(time.Second), 10)
	jobs = slices.DeleteFunc(jobs, func(job deliveryJob) bool { return job.message.Kind != push.Alert })
	if err != nil || len(jobs) != want {
		t.Fatalf("claims = %d, want %d, error = %v", len(jobs), want, err)
	}
	return jobs
}

func assertFlightAlertSends(t *testing.T, store *Store, jobs []deliveryJob, wantPerPlatform ...string) {
	t.Helper()
	var sent, want []string
	sender := pushFunc(func(_ context.Context, target push.Target, message push.Message) error {
		if (message.AlertKind != "") != (target.Platform == "android") {
			t.Fatalf("flight event kind %q for %s", message.AlertKind, target.Platform)
		}
		if (message.AlertPhase != "") != (target.Platform == "android") {
			t.Fatalf("flight event phase %q for %s", message.AlertPhase, target.Platform)
		}
		sent = append(sent, target.Platform+"|"+message.Title+"|"+message.Body)
		return nil
	})
	logger := slog.New(slog.DiscardHandler)
	for _, job := range jobs {
		store.deliver(t.Context(), sender, logger, job)
	}
	for _, message := range wantPerPlatform {
		want = append(want, "ios|"+message, "android|"+message)
	}
	slices.Sort(sent)
	slices.Sort(want)
	if !slices.Equal(sent, want) {
		t.Fatalf("alerts sent = %q, want %q", sent, want)
	}
}

func assertFlightAlertHistory(t *testing.T, store *Store, id string, want ...string) {
	t.Helper()
	details, err := store.Details(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, event := range details.Events {
		events = append(events, event.Kind+"|"+event.Body)
	}
	slices.Sort(events)
	slices.Sort(want)
	if !slices.Equal(events, want) {
		t.Fatalf("flight history = %q, want %q", events, want)
	}
}
