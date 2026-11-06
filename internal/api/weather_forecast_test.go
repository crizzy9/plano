package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestFlightDetailsShowTerminalForecastForTrackedOperation(t *testing.T) {
	service := newImportTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	start, end := now.Add(3*time.Hour), now.Add(5*time.Hour)
	flight := feeds.Flight{
		ID: "terminal-weather-fixture", Number: "UA123", Source: "flightaware_aeroapi", Status: "scheduled",
		Origin: "SFO", OriginICAO: "KSFO", Destination: "JFK", DestinationICAO: "KJFK",
		ScheduledDeparture: new(now.Add(4 * time.Hour)), ScheduledArrival: new(now.Add(10 * time.Hour)),
		ObservedAt: now,
	}
	token, owner := newImportTestInstallation(t, service.Store)
	otherToken, _ := newImportTestInstallation(t, service.Store)
	if err := service.Store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.Track(t.Context(), owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	stationBatch := make(chan string, 1)
	client := &http.Client{Transport: rotationReceiverTransport(func(r *http.Request) (*http.Response, error) {
		stationBatch <- r.URL.Query().Get("ids")
		body := fmt.Sprintf(`[{"icaoId":"KSFO","issueTime":%q,"dbPopTime":%q,"validTimeFrom":%d,"validTimeTo":%d,"mostRecent":1,"rawTAF":"KSFO 060000Z","fcsts":[{"timeFrom":%d,"timeTo":%d,"fcstChange":"TEMPO","probability":30,"wxString":"TSRA"}]}]`,
			now.Format(time.RFC3339), now.Format(time.RFC3339), now.Unix(), end.Unix(), start.Unix(), end.Unix())
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	service.WeatherForecast = core.NewWeatherForecastCache(feeds.NewTAF(client))
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		service.WeatherForecast.Run(ctx, service.Store, service.Logger)
	}()
	defer func() { cancel(); <-stopped }()
	select {
	case got := <-stationBatch:
		if got != "KJFK,KSFO" {
			t.Fatalf("TAF station request = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("weather worker did not request a station batch")
	}
	handler := service.Handler()
	path := "/v1/flights/" + flight.ID
	var page struct {
		WeatherForecasts []core.AirportWeatherForecast `json:"weatherForecasts"`
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		data := importTestRequest(t, handler, http.MethodGet, path, token, "", http.StatusOK)
		if err := json.Unmarshal([]byte(data), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.WeatherForecasts) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(page.WeatherForecasts) != 1 {
		t.Fatalf("flight detail forecast = %+v", page.WeatherForecasts)
	}
	forecast := page.WeatherForecasts[0]
	if forecast.Airport != "KSFO" || forecast.Operation != "departure" || forecast.Conditions != "thunderstorms" ||
		forecast.Probability != 30 || !forecast.IssuedAt.Equal(now) || !forecast.StartsAt.Equal(start) || !forecast.EndsAt.Equal(end) {
		t.Fatalf("unexpected forecast %+v", forecast)
	}
	if data := importTestRequest(t, handler, http.MethodGet, path, otherToken, "", http.StatusOK); strings.Contains(data, `"weatherForecasts"`) {
		t.Fatal("untracked flight detail included the forecast")
	}
}
