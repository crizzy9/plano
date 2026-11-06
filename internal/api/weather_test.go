package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestFlightWeatherIsVisibleOnlyWhileTrackedByInstallation(t *testing.T) {
	service := newImportTestServer(t)
	token, owner := newImportTestInstallation(t, service.Store)
	otherToken, _ := newImportTestInstallation(t, service.Store)
	now := time.Now().UTC()
	flight := feeds.Flight{ID: "test-weather-flight", Number: "UA123", Status: "scheduled", Source: "fixture",
		OriginICAO: "KSFO", DestinationICAO: "KJFK", ScheduledDeparture: new(now.Add(30 * time.Minute)),
		ScheduledArrival: new(now.Add(6 * time.Hour)), ObservedAt: now}
	if err := service.Store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.Track(t.Context(), owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	report := fmt.Sprintf(`[{"icaoId":"KSFO","obsTime":%d,"visib":1}]`, now.Add(-time.Minute).Unix())
	service.Weather = core.NewWeatherCache(feeds.NewMETAR(&http.Client{Transport: tripItTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(report))}, nil
	})}))
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		service.Weather.Run(ctx, service.Store, service.Logger)
	}()
	t.Cleanup(func() { cancel(); <-finished })
	deadline := time.Now().Add(2 * time.Second)
	for len(service.Weather.ForFlight(flight, time.Now())) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("weather fixture was not cached")
		}
		time.Sleep(5 * time.Millisecond)
	}
	handler := service.Handler()
	path := "/v1/flights/" + flight.ID
	if body := importTestRequest(t, handler, "GET", path, token, "", 200); !strings.Contains(body, `"weather":[{"airport":"KSFO"`) {
		t.Fatalf("tracking owner did not receive conditions: %s", body)
	}
	if body := importTestRequest(t, handler, "GET", path, otherToken, "", 200); strings.Contains(body, `"weather":`) {
		t.Fatalf("another installation could read cached conditions: %s", body)
	}
	importTestRequest(t, handler, "DELETE", "/v1/tracked/"+flight.ID, token, "", 204)
	if body := importTestRequest(t, handler, "GET", path, token, "", 200); strings.Contains(body, `"weather":`) {
		t.Fatalf("weather remains accessible after untracking: %s", body)
	}
}
