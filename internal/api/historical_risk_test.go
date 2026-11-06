package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/historicalrisk"
)

func TestFlightDetailsHistoricalRiskUsesScheduledOriginLocalTime(t *testing.T) {
	now := time.Now().UTC()
	date := now.AddDate(0, 0, -14).Format(time.DateOnly)
	publishedOn, err := time.Parse(time.DateOnly, now.AddDate(0, 0, -7).Format(time.DateOnly))
	if err != nil {
		t.Fatal(err)
	}
	var csv strings.Builder
	csv.WriteString("FlightDate,Origin,Dest,CRSDepTime,DepDelay,Cancelled\n")
	for range 1000 {
		fmt.Fprintf(&csv, "%s,EWR,SFO,0800,0,0\n%s,EWR,SFO,1700,30,0\n", date, date)
	}
	model, err := historicalrisk.Fit(strings.NewReader(csv.String()), publishedOn)
	if err != nil {
		t.Fatal(err)
	}
	store := openPassportStore(t, filepath.Join(t.TempDir(), "historical-risk.db"))
	token, _ := passportInstallation(t, store)
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	year, month, day := now.In(loc).AddDate(0, 0, 2).Date()
	departure := time.Date(year, month, day, 8, 0, 0, 0, loc).UTC()
	base := feeds.Flight{Number: "UA123", Origin: "EWR", Destination: "SFO", Status: "scheduled", OriginTimeZone: loc.String(),
		ObservedAt: now, ScheduledDeparture: &departure}
	for _, tc := range []struct {
		name, destination, zone, status string
		demo, want                      bool
	}{
		{"supported", "SFO", loc.String(), "scheduled", false, true},
		{"unsupported", "LHR", loc.String(), "scheduled", false, false},
		{"missing-zone", "SFO", "", "scheduled", false, false},
		{"bad-zone", "SFO", "../etc/localtime", "scheduled", false, false},
		{"departed", "SFO", loc.String(), "airborne", false, false},
		{"demo", "SFO", loc.String(), "scheduled", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flight := base
			flight.ID, flight.Destination, flight.OriginTimeZone, flight.Status = tc.name, tc.destination, tc.zone, tc.status
			if err := store.Observe(t.Context(), flight, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/v1/flights/"+flight.ID, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp := httptest.NewRecorder()
			(Server{Store: store, Demo: tc.demo, HistoricalRisk: model}).Handler().ServeHTTP(resp, req)
			var result struct {
				Risk *historicalrisk.Estimate `json:"historicalDepartureRisk"`
			}
			if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil || resp.Code != 200 {
				t.Fatalf("GET details returned %d, decode error = %v", resp.Code, err)
			}
			if (result.Risk != nil) != tc.want {
				t.Fatalf("risk = %+v, want presence %v", result.Risk, tc.want)
			}
			if tc.want && (result.Risk.Probability >= 0.2 || result.Risk.SampleSize != 1000 || result.Risk.TrainingThrough != date) {
				t.Fatalf("incorrect local departure cohort: %+v", result.Risk)
			}
		})
	}
}
