package core

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestPeriodicRefreshRecoversInterruptedRotationHistory(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "rotation-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	seen := now.Add(-5 * time.Minute)
	const source = "flightaware_aeroapi"
	flight := feeds.Flight{
		ID: "UAL900", Number: "UA900", Origin: "SFO", Destination: "JFK", Status: "scheduled",
		TailNumber: "N123AA", InboundID: "UAL901", Source: source, ObservedAt: seen,
		ScheduledDeparture: new(now.Add(time.Hour)), ScheduledArrival: new(now.Add(7 * time.Hour)),
	}
	direct := feeds.Flight{
		ID: "UAL901", Number: "UA901", Origin: "DEN", Destination: "SFO", Status: "airborne",
		TailNumber: flight.TailNumber, InboundID: "UAL902", Source: source, ObservedAt: seen,
		ScheduledDeparture: new(now.Add(-2 * time.Hour)), ScheduledArrival: new(now.Add(20 * time.Minute)),
	}
	earlier := feeds.Flight{
		ID: "UAL902", Number: "UA902", Origin: "ORD", Destination: "DEN", Status: "arrived",
		TailNumber: flight.TailNumber, Source: source, ObservedAt: seen,
		ScheduledDeparture: new(now.Add(-6 * time.Hour)), ActualArrival: new(now.Add(-3 * time.Hour)),
	}
	if err := store.Observe(ctx, flight, nil, []feeds.Flight{direct, earlier}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, friendInstallation(t, store), flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Exercise the real adapter to distinguish an interrupted explicit lookup
	// from a complete response with no older connecting leg.
	oldestStatus := http.StatusServiceUnavailable
	provider := feeds.NewAeroAPI("fixture-key", &http.Client{Transport: aeroCallbackTransport(func(req *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, `{"flights":[]}`
		switch req.URL.Path {
		case "/aeroapi/flights/UAL900":
			body = fmt.Sprintf(`{"flights":[{"fa_flight_id":"UAL900","ident_iata":"UA900","registration":"N123AA","inbound_fa_flight_id":"UAL901",
				"origin":{"code_iata":"SFO"},"destination":{"code_iata":"JFK"},"scheduled_out":%q,"scheduled_in":%q}]}`,
				flight.ScheduledDeparture.Format(time.RFC3339), flight.ScheduledArrival.Format(time.RFC3339))
		case "/aeroapi/flights/UAL901":
			body = fmt.Sprintf(`{"flights":[{"fa_flight_id":"UAL901","ident_iata":"UA901","registration":"N123AA","inbound_fa_flight_id":"UAL902",
				"origin":{"code_iata":"DEN"},"destination":{"code_iata":"SFO"},"status":"En Route","scheduled_out":%q,"scheduled_in":%q,"estimated_in":%q}]}`,
				direct.ScheduledDeparture.Format(time.RFC3339), direct.ScheduledArrival.Format(time.RFC3339), now.Add(40*time.Minute).Format(time.RFC3339))
		case "/aeroapi/flights/UAL902":
			status = oldestStatus
		case "/aeroapi/history/flights/UAL902":
			status = http.StatusNotFound
		case "/aeroapi/flights/N123AA":
			// The completed lookup confirms an empty registration history.
		default:
			return nil, fmt.Errorf("unexpected rotation request %s", req.URL.Path)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	refresher := Refresher{Store: store, Flights: provider, Logger: slog.New(slog.DiscardHandler)}
	if err := refresher.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Details(ctx, flight.ID)
	if err != nil || len(detail.Inbound) != 2 || detail.Inbound[0].ID != direct.ID ||
		!detail.Inbound[0].ObservedAt.After(direct.ObservedAt) || detail.Inbound[1].ID != earlier.ID ||
		!detail.Inbound[1].ObservedAt.Equal(earlier.ObservedAt) {
		t.Fatalf("partial rotation = %+v, error = %v", detail.Inbound, err)
	}
	if detail.Prediction == nil || detail.Prediction.DelayMinutes != 20 {
		t.Fatalf("prediction did not use the fresh direct inbound ETA: %+v", detail.Prediction)
	}

	oldestStatus = http.StatusNotFound
	if err := refresher.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err = store.Details(ctx, flight.ID)
	if err != nil || len(detail.Inbound) != 1 || detail.Inbound[0].ID != direct.ID {
		t.Fatalf("complete shorter rotation = %+v, error = %v", detail.Inbound, err)
	}

	// A newer callback may correct the direct leg while the poll is fetching
	// older history. The old prefix cannot establish that callback's suffix.
	callback := detail.Inbound[0]
	callback.ObservedAt = callback.ObservedAt.Add(time.Second)
	callback.ActualDeparture = new(now.Add(-4 * time.Hour))
	cached, err := json.Marshal([]feeds.Flight{callback, earlier})
	if err != nil {
		t.Fatal(err)
	}
	reconciled, retained, err := keepNewerAeroInbound(detail.Flight, detail.Inbound, cached, true)
	if err != nil || !retained || len(reconciled) != 1 || !reconciled[0].ObservedAt.Equal(callback.ObservedAt) {
		t.Fatalf("rotation after callback correction = %+v, retained = %t, error = %v", reconciled, retained, err)
	}
}
