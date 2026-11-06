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

func TestFlightAlertDispatcherRefreshesQueuedDelayEstimate(t *testing.T) {
	store, flight := flightAlertDispatchFixture(t)
	ctx := t.Context()
	flight.InboundID = "direct-inbound"
	flight.ScheduledDeparture = new(flight.ObservedAt.Add(30 * time.Hour))
	inbound := feeds.Flight{ID: flight.InboundID, Number: "AA1", Destination: flight.Origin, Status: "airborne"}
	observe := func(delay int) {
		t.Helper()
		flight.ObservedAt = flight.ObservedAt.Add(time.Nanosecond)
		inbound.ObservedAt = flight.ObservedAt
		inbound.EstimatedArrival = new(flight.ScheduledDeparture.Add(time.Duration(delay-40) * time.Minute))
		if err := store.observeTracked(ctx, flight, nil, []feeds.Flight{inbound}, nil); err != nil {
			t.Fatal(err)
		}
	}
	observe(50)
	jobs := claimFlightAlertJobs(t, store, 2)
	for _, delay := range []int{41, 32, 23, 14} {
		observe(delay)
	}
	details, err := store.Details(ctx, flight.ID)
	if err != nil || details.Prediction == nil || details.Prediction.DelayMinutes != 14 {
		t.Fatalf("latest forecast = %+v, error = %v", details.Prediction, err)
	}
	assertFlightAlertSends(t, store, jobs, "Departure delay predicted|AA2 · 14 min later")
	claimFlightAlertJobs(t, store, 0)
	assertFlightAlertHistory(t, store, flight.ID, "delay_prediction|AA2 · 50 min later")
}

func TestFlightAlertDispatcherUsesRevisedLookupEstimate(t *testing.T) {
	store, flight := flightAlertDispatchFixture(t)
	flight.InboundID = "lookup-inbound"
	flight.ScheduledDeparture = new(flight.ObservedAt.Add(30 * time.Hour))
	inbound := feeds.Flight{ID: flight.InboundID, Number: "AA1", Destination: flight.Origin, Status: "airborne",
		ObservedAt: flight.ObservedAt, EstimatedArrival: new(flight.ScheduledDeparture.Add(-20 * time.Minute))}
	flight.ObservedAt = flight.ObservedAt.Add(time.Millisecond)
	if err := store.observeTracked(t.Context(), flight, nil, []feeds.Flight{inbound}, nil); err != nil {
		t.Fatal(err)
	}
	jobs := claimFlightAlertJobs(t, store, 2)
	// Four minutes stays below the threshold for a new delay prediction.
	// The original forecast event remains, with the latest estimate sent.
	flight.EstimatedDeparture = new(flight.ScheduledDeparture.Add(24 * time.Minute))
	observeFlightAlertFixture(t, store, &flight)
	details, err := store.Details(t.Context(), flight.ID)
	if err != nil || details.Prediction == nil || details.Prediction.DelayMinutes != 24 {
		t.Fatalf("forecast after lookup = %+v, error = %v", details.Prediction, err)
	}
	assertFlightAlertSends(t, store, jobs, "Departure delay predicted|AA2 · 24 min later")
	assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2), "Departure estimate changed|AA2 · 24 min later")
	assertFlightAlertHistory(t, store, flight.ID, "delay_prediction|AA2 · 20 min later", "etd|AA2 · 24 min later")
}

func TestFlightAlertDispatcherRechecksLatestDelayAdvisory(t *testing.T) {
	store, flight := flightAlertDispatchFixture(t)
	ctx := t.Context()
	flight.ScheduledDeparture = new(flight.ObservedAt.Add(30 * time.Hour))
	advisory := feeds.Advisory{Source: feeds.FAASWIM, ID: "original-delay", Kind: "departure_delay", Airport: flight.Origin,
		Direction: "departure", Reason: "weather", DelayMinutes: new(50), ObservedAt: flight.ObservedAt}
	observe := func() {
		t.Helper()
		flight.ObservedAt = flight.ObservedAt.Add(time.Nanosecond)
		if err := store.observeTracked(ctx, flight, nil, nil, []feeds.Advisory{advisory}); err != nil {
			t.Fatal(err)
		}
	}
	observe()
	jobs := slices.DeleteFunc(claimFlightAlertJobs(t, store, 4), func(job deliveryJob) bool {
		return job.message.Title != "Departure delay predicted"
	})
	if len(jobs) != 2 {
		t.Fatalf("claimed delay warnings = %d, want 2", len(jobs))
	}

	// A different advisory with the same reason can change the saved forecast
	// without creating another prediction event. Only the forecast cites it.
	advisory.ID, advisory.DelayMinutes = "latest-delay", new(41)
	observe()
	details, err := store.Details(ctx, flight.ID)
	if err != nil || details.Prediction == nil || details.Prediction.DelayMinutes != 41 {
		t.Fatalf("latest forecast = %+v, error = %v", details.Prediction, err)
	}
	withdrawnAt := time.Now().UTC()
	advisory.Kind, advisory.ObservedAt = "cancelled", withdrawnAt
	if err := store.PublishAdvisory(ctx, advisory, withdrawnAt); err != nil {
		t.Fatal(err)
	}
	assertFlightAlertSends(t, store, jobs)
	assertFlightAlertHistory(t, store, flight.ID, "delay_prediction|AA2 · 50 min later", "departure_delay|weather", "departure_delay|weather")
}

func TestFlightAlertDispatcherRechecksLegacyInboundForecast(t *testing.T) {
	for _, tc := range []struct {
		name     string
		offset   time.Duration
		wantSent int
	}{
		{name: "elapsed ETA", offset: -time.Minute},
		{name: "future ETA caps expiry", offset: 5 * time.Minute, wantSent: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, flight := flightAlertDispatchFixture(t)
			ctx := t.Context()
			now := time.Now().UTC()
			eta := now.Add(tc.offset)
			flight.InboundID, flight.ScheduledDeparture = "direct-inbound", new(eta.Add(-15*time.Minute))
			flight.ObservedAt = flight.ObservedAt.Add(time.Millisecond)
			inbound := feeds.Flight{ID: flight.InboundID, Number: "AA1", Destination: flight.Origin, Status: "airborne",
				ObservedAt: now.Add(-2 * time.Minute), EstimatedArrival: &eta}
			if err := store.Observe(ctx, flight, nil, []feeds.Flight{inbound}, nil); err != nil {
				t.Fatal(err)
			}
			// Older forecast writers could save this estimate without a deadline.
			forecast, err := json.Marshal(Prediction{Departure: new(eta.Add(40 * time.Minute)), DelayMinutes: 55,
				Reasons: []string{inboundReasonPrefix + "AA1 needs time to arrive and turn around."}, Method: "operational_estimate"})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, `UPDATE flights SET prediction=? WHERE id=?`, string(forecast), flight.ID); err != nil {
				t.Fatal(err)
			}
			const body = "AA2 · 55 min later"
			if err := saveEvent(ctx, tx, flight.ID, Event{Kind: "delay_prediction", Title: "Departure delay predicted",
				Body: body, At: inbound.ObservedAt}, true); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var sent int
			sender := pushFunc(func(_ context.Context, _ push.Target, message push.Message) error {
				sent++
				if tc.wantSent > 0 && !message.ExpiresAt.Equal(eta) {
					t.Errorf("legacy push expiry = %s, want %s", message.ExpiresAt, eta)
				}
				return nil
			})
			logger := slog.New(slog.DiscardHandler)
			for _, job := range claimFlightAlertJobs(t, store, 2) {
				if !job.message.ExpiresAt.After(eta) {
					t.Fatalf("test job expires at %s before legacy evidence deadline %s", job.message.ExpiresAt, eta)
				}
				store.deliver(ctx, sender, logger, job)
			}
			var pending int
			err = store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox o`+outboxFlightEvent+`
WHERE o.flight_id=? AND o.done=0 AND json_extract(e.data,'$.kind')='delay_prediction'`, flight.ID).Scan(&pending)
			if sent != tc.wantSent || err != nil || pending != 0 {
				t.Fatalf("sent %d, want %d, pending %d, error %v", sent, tc.wantSent, pending, err)
			}
			assertFlightAlertHistory(t, store, flight.ID, "delay_prediction|"+body)
		})
	}
}

func TestFlightAlertDispatcherRechecksTerminalWeatherForecastAmendment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		amended bool
	}{
		{name: "current forecast delivers"},
		{name: "amended forecast retires leased alerts", amended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, flight := flightAlertDispatchFixture(t)
			ctx := t.Context()
			now := time.Now().UTC()
			flight.OriginICAO, flight.ScheduledDeparture = "KJFK", new(now.Add(3*time.Hour))
			period := feeds.TAFPeriod{
				Airport: "KJFK", Conditions: "thunderstorms", Qualifier: "prevailing",
				IssuedAt: now.Add(-time.Hour), StartsAt: now.Add(2 * time.Hour), EndsAt: now.Add(4 * time.Hour),
			}
			cache := NewWeatherForecastCache(nil)
			cache.byStation["KJFK"] = stationWeatherForecast{availableAt: now, periods: []feeds.TAFPeriod{period}}
			store.weatherForecast = cache
			forecasts := cache.ForFlight(flight, now)
			if len(forecasts) != 1 {
				t.Fatalf("weather fixture returned %d forecasts, want 1", len(forecasts))
			}
			forecast, err := json.Marshal(Prediction{
				Departure: new(flight.ScheduledDeparture.Add(45 * time.Minute)), ValidUntil: &period.EndsAt, DelayMinutes: 45,
				Reasons: []string{"NOAA forecasts thunderstorms at KJFK during the expected departure."}, Method: "learned_estimate",
				TerminalWeatherEvidence: &terminalWeatherEvidence{Forecasts: forecasts},
			})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := json.Marshal(flight)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, `UPDATE flights SET snapshot=?, prediction=? WHERE id=?`, string(snapshot), string(forecast), flight.ID); err != nil {
				t.Fatal(err)
			}
			const title, body = "Departure delay predicted", "AA2 · 45 min later"
			if err := saveEvent(ctx, tx, flight.ID, Event{Kind: "delay_prediction", Title: title, Body: body, At: now}, true); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			jobs := claimFlightAlertJobs(t, store, 2)
			if tc.amended {
				// The amendment arrived after claim, with the same hazard text
				// but a different issuance. The saved forecast still uses the old one.
				period.IssuedAt = now.Add(-time.Minute)
				cache.byStation["KJFK"] = stationWeatherForecast{availableAt: time.Now().UTC(), periods: []feeds.TAFPeriod{period}}
				assertFlightAlertSends(t, store, jobs)
			} else {
				assertFlightAlertSends(t, store, jobs, title+"|"+body)
			}
			var pending int
			err = store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE flight_id=? AND done=0`, flight.ID).Scan(&pending)
			if err != nil || pending != 0 {
				t.Fatalf("pending delay jobs = %d, error = %v", pending, err)
			}
			assertFlightAlertHistory(t, store, flight.ID, "delay_prediction|"+body)
		})
	}
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

func TestFlightAlertDispatcherReoffersOnlyUnsentCheckInAfterCorrection(t *testing.T) {
	for _, firstClaim := range []string{"unsent", "reclaimed unsent", "legacy", "legacy reclaims", "submitted"} {
		t.Run(firstClaim, func(t *testing.T) {
			store, flight := flightAlertDispatchFixture(t)
			departure := flight.ObservedAt.Add(time.Hour)
			flight.ScheduledDeparture = &departure
			observeFlightAlertFixture(t, store, &flight)
			claims := claimFlightAlertJobs(t, store, 2)
			correct := func() {
				flight.ScheduledDeparture = new(flight.ObservedAt.Add(30 * time.Hour))
				observeFlightAlertFixture(t, store, &flight)
			}
			logger := slog.New(slog.DiscardHandler)
			switch firstClaim {
			case "reclaimed unsent":
				var err error
				claims, err = store.claim(t.Context(), time.Unix(0, claims[0].lease).Add(time.Second), len(claims))
				if err != nil || len(claims) != 2 {
					t.Fatalf("reclaims = %d, want 2, error = %v", len(claims), err)
				}
				correct()
			case "legacy":
				if _, err := store.db.ExecContext(t.Context(), `DELETE FROM outbox_check_in_leases WHERE outbox_id=?`, claims[0].id); err != nil {
					t.Fatal(err)
				}
				correct()
			case "legacy reclaims":
				claims[0].lease += int64(time.Minute)
				if _, err := store.db.ExecContext(t.Context(), `UPDATE outbox SET lease_until=? WHERE id=?`, claims[0].lease, claims[0].id); err != nil {
					t.Fatal(err)
				}
				correct()
			case "submitted":
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				submitted := false
				store.deliver(ctx, pushFunc(func(context.Context, push.Target, push.Message) error {
					submitted = true
					correct()
					cancel()
					return context.Canceled
				}), logger, claims[0])
				if !submitted {
					t.Fatal("fixture check-in did not reach transport")
				}
			default:
				correct()
			}
			for _, claim := range claims {
				if authorized, err := store.authorizeCheckIn(t.Context(), claim, time.Now()); err != nil || authorized {
					t.Fatalf("corrected check-in lease authorized = %t, error = %v", authorized, err)
				}
			}
			assertFlightAlertSends(t, store, claims)
			flight.ScheduledDeparture = &departure
			observeFlightAlertFixture(t, store, &flight)
			want := []string{claims[1].target.Platform}
			if firstClaim == "unsent" || firstClaim == "reclaimed unsent" {
				want = append(want, claims[0].target.Platform)
			}
			var sent []string
			for _, job := range claimFlightAlertJobs(t, store, len(want)) {
				store.deliver(t.Context(), pushFunc(func(_ context.Context, target push.Target, message push.Message) error {
					if message.Title != "Check-in reminder" || message.CollapseID != claims[0].message.CollapseID {
						t.Fatalf("unexpected replacement alert %#v", message)
					}
					sent = append(sent, target.Platform)
					return nil
				}), logger, job)
			}
			slices.Sort(want)
			slices.Sort(sent)
			if !slices.Equal(sent, want) {
				t.Fatalf("check-in recipients = %v, want %v", sent, want)
			}
		})
	}
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
