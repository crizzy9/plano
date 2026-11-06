package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func TestConnectionMonitorAPIAndWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store, err := core.Open(filepath.Join(t.TempDir(), "plano.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	arrival, departure := now.Add(time.Hour), now.Add(3*time.Hour)
	in := feeds.Flight{ID: "monitor-in", Number: "PL1", Origin: "SFO", Destination: "DEN", Status: "scheduled", ScheduledArrival: &arrival, ObservedAt: now}
	out := feeds.Flight{ID: "monitor-out", Number: "PL2", Origin: "DEN", Destination: "EWR", Status: "scheduled", ScheduledDeparture: &departure, ObservedAt: now}
	for _, f := range []feeds.Flight{in, out} {
		if err := store.Observe(ctx, f, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	createOwner := func(device string, both bool) string {
		t.Helper()
		token, err := store.CreateInstallation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := store.Installation(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Track(ctx, owner, out.ID, ""); err != nil {
			t.Fatal(err)
		}
		if both {
			if err := store.Track(ctx, owner, in.ID, ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Register(ctx, owner, core.Device{Platform: "ios", Kind: "alert", Token: device}, now); err != nil {
			t.Fatal(err)
		}
		return token
	}
	owner, other := createOwner("monitor-owner", true), createOwner("monitor-other", true)
	partial := createOwner("monitor-partial", false)
	service := Server{Store: store, Logger: slog.New(slog.DiscardHandler)}
	handler := service.Handler()
	request := func(method, path, token string, want int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, res.Code, res.Body, want)
		}
		return res.Body.Bytes()
	}
	const query = "?inbound=monitor-in&outbound=monitor-out"
	const monitors = "/v1/connection-monitors"
	status := func(token string) connectionResult {
		t.Helper()
		var result connectionResult
		if err := json.Unmarshal(request(http.MethodGet, "/v1/connections"+query, token, http.StatusOK), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	list := func(token string, count int) []core.ConnectionMonitor {
		t.Helper()
		var result struct{ Monitors []core.ConnectionMonitor }
		if err := json.Unmarshal(request(http.MethodGet, monitors, token, http.StatusOK), &result); err != nil || result.Monitors == nil || len(result.Monitors) != count {
			t.Fatalf("monitor list has wrong shape or count: %+v %v", result, err)
		}
		return result.Monitors
	}
	request(http.MethodPut, monitors+query, "", http.StatusUnauthorized)
	request(http.MethodPut, monitors+query, partial, http.StatusBadRequest)
	request(http.MethodPut, monitors+"?inbound=monitor-out&outbound=monitor-in", owner, http.StatusBadRequest)
	request(http.MethodGet, "/v1/tracked?flightId=", owner, http.StatusBadRequest)
	request(http.MethodGet, "/v1/tracked?flightId=monitor-in&flightId=monitor-out&flightId=third", owner, http.StatusBadRequest)
	var targets struct{ Flights []feeds.Flight }
	if err := json.Unmarshal(request(http.MethodGet, "/v1/tracked?flightId=monitor-in&flightId=monitor-out", partial, http.StatusOK), &targets); err != nil ||
		len(targets.Flights) != 1 || targets.Flights[0].ID != out.ID {
		t.Fatalf("filtered lookup exposed an untracked leg: %+v %v", targets, err)
	}
	if result := status(owner); !result.MonitorEligible || result.Monitoring || result.Risk != "Relaxed" {
		t.Fatalf("unexpected baseline: %+v", result)
	}
	request(http.MethodPut, monitors+query, owner, http.StatusOK)
	request(http.MethodPut, monitors+query, owner, http.StatusOK)
	if result := status(owner); !result.Monitoring {
		t.Fatal("monitor not shown on the assessed pair")
	}
	if saved := list(owner, 1)[0]; saved.Inbound != in.ID || saved.Outbound != out.ID || saved.Risk != "Relaxed" {
		t.Fatalf("wrong saved monitor: %+v", saved)
	}
	request(http.MethodDelete, monitors+query, other, http.StatusNoContent)
	list(other, 0)
	list(owner, 1)

	workerDone, dispatcherDone := make(chan struct{}), make(chan struct{})
	alerts := make(connectionPushSink, 8)
	go func() { defer close(workerDone); service.RunConnectionMonitors(ctx) }()
	go func() { defer close(dispatcherDone); store.RunDispatcher(ctx, alerts, service.Logger) }()
	defer func() { cancel(); <-workerDone; <-dispatcherDone }()
	late := departure.Add(time.Minute)
	in.EstimatedArrival, in.ObservedAt = &late, now.Add(time.Second)
	if err := store.Observe(ctx, in, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case alert := <-alerts:
		if alert.target != "monitor-owner" || alert.message.Kind != push.Alert || alert.message.FlightID != out.ID {
			t.Fatalf("unexpected connection alert: %+v", alert)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connection warning did not reach the fake transport")
	}
	if result := status(owner); result.Risk != "Missed" || !result.Monitoring {
		t.Fatalf("live time inversion should remain monitored: %+v", result)
	}
	if saved := list(owner, 1)[0]; saved.Risk != "Missed" {
		t.Fatalf("worker and HTTP assessment disagreed: %+v", saved)
	}
	request(http.MethodDelete, monitors+query, owner, http.StatusNoContent)
	request(http.MethodDelete, monitors+query, owner, http.StatusNoContent)
	list(owner, 0)
}

type connectionPush struct {
	target  string
	message push.Message
}

type connectionPushSink chan connectionPush

func (sink connectionPushSink) Send(ctx context.Context, target push.Target, message push.Message) error {
	if !strings.Contains(strings.ToLower(message.Title), "connection") {
		return nil
	}
	select {
	case sink <- connectionPush{target.Token, message}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestConnectionEvaluationClockAndTerminalDisruption(t *testing.T) {
	now := time.Date(2026, time.November, 5, 12, 0, 0, 0, time.UTC)
	arrival, departure := now.Add(-time.Minute), now.Add(92*time.Minute)
	in := feeds.Flight{Destination: "DEN", ScheduledArrival: &arrival, ObservedAt: now}
	out := feeds.Flight{Origin: "DEN", ScheduledDeparture: &departure, ObservedAt: now}
	noGate := func(context.Context, feeds.Flight, bool, time.Time) (*core.GatePrediction, error) { return nil, nil }
	service := Server{}
	assessment, err := service.evaluateConnection(t.Context(), in, out, now, noGate)
	wantCheck := now.Add(2*time.Minute + time.Millisecond)
	if err != nil || assessment.Risk != "Relaxed" || !assessment.NextCheck.Equal(wantCheck) {
		t.Fatalf("unexpected clock schedule: %+v %v", assessment, err)
	}
	assessment, err = service.evaluateConnection(t.Context(), in, out, wantCheck, noGate)
	if err != nil || assessment.Risk != "Tight" || !assessment.Monitorable {
		t.Fatalf("clock alone did not worsen the risk: %+v %v", assessment, err)
	}
	partial := in
	partial.ScheduledArrival = nil
	assessment, err = service.evaluateConnection(t.Context(), partial, out, now, noGate)
	if err != nil || !assessment.Incomplete {
		t.Fatalf("partial feed data should leave a watch eligible for reevaluation: %+v %v", assessment, err)
	}
	gateIn, takeoff := now.Add(-10*time.Minute), now.Add(-15*time.Minute)
	in.ActualArrival, out.ActualTakeoff = &gateIn, &takeoff
	assessment, err = service.evaluateConnection(t.Context(), in, out, now, noGate)
	if err != nil || assessment.Risk != "Missed" || assessment.Monitorable {
		t.Fatalf("stale scheduled departure overrode actual takeoff: %+v %v", assessment, err)
	}
	in.Status, in.Destination, in.ScheduledArrival = "diverted", "DFW", nil
	assessment, err = service.evaluateConnection(t.Context(), in, out, now, noGate)
	if err != nil || assessment.Risk != "Missed" || assessment.Monitorable || !assessment.AlertUntil.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("lost the terminal alert after diversion: %+v %v", assessment, err)
	}
}
