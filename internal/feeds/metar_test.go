package feeds

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMETARObservationsUseLatestRequestedStationReports(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	fixture := fmt.Sprintf(`[
		{"icaoId":"KSFO","obsTime":%d,"wxString":"VCTSRA -FZDZ VCTS","visib":"0.75","wspd":26,"wgst":39,
		 "clouds":[{"cover":"SCT","base":100},{"cover":"BKN","base":900},{"cover":"OVC","base":550}],"rawOb":"ignored"},
		{"icaoId":"KLAX","obsTime":%d,"wxString":"TSRA"},
		{"icaoId":"KLAX","obsTime":%d,"visib":"10+","clouds":[{"cover":"SCT","base":600}]},
		{"icaoId":"KJFK","obsTime":%d,"receiptTime":"2023-11-14 22:13:05.000Z","visib":0.25},
		{"icaoId":"KJFK","obsTime":%d,"receiptTime":"2023-11-14T22:13:00Z","visib":0.75},
		{"icaoId":"KEWR","obsTime":%d,"wxString":"TSRA"},
		{"icaoId":"KSEA","obsTime":%d,"wxString":"TSRA"},
		{"icaoId":"KDEN","obsTime":%d,"wxString":"TSRA"}
	]`, now.Unix()-120, now.Unix()-120, now.Unix()-30, now.Unix()-120, now.Unix()-120, now.Unix()+1, now.Add(-metarValidity).Unix(), now.Unix()-30)
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		query := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "aviationweather.gov" || r.URL.Path != "/api/data/metar" ||
			query.Get("format") != "json" || query.Get("ids") != "KEWR,KJFK,KLAX,KSEA,KSFO" || !strings.HasPrefix(r.UserAgent(), "Plano/") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		return response(http.StatusOK, fixture)
	})}
	result, err := NewMETAR(client).Observations(t.Context(), []string{"KSFO", "KJFK", "KLAX", " ksfo ", "KSEA", "KEWR"}, now)
	if err != nil || len(result) != 2 {
		t.Fatalf("observations = %#v, %v", result, err)
	}
	if got := result[0]; got.Airport != "KJFK" || got.Reason != "low visibility at 0.25 statute miles" {
		t.Fatalf("corrected observation = %#v", got)
	}
	got := result[1]
	if got.Airport != "KSFO" || got.Kind != METARWeatherObservation || got.Source != METARSource || got.DelayMinutes != nil || got.RawReason != "" ||
		got.ObservedAt != now.Add(-2*time.Minute) || got.StartsAt == nil || *got.StartsAt != got.ObservedAt || got.EndsAt == nil || *got.EndsAt != got.ObservedAt.Add(metarValidity) ||
		got.Reason != "thunderstorms in the vicinity, freezing drizzle, a low ceiling at 550 ft, low visibility at 0.75 statute miles, sustained winds at 26 kt, gusts up to 39 kt" {
		t.Fatalf("adverse observation = %#v", got)
	}
}

func TestMETARRejectsInvalidOrOversizedStationQueriesWithoutHTTP(t *testing.T) {
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected HTTP request for rejected or empty query: %s", r.URL)
		return nil, nil
	})}
	feed := NewMETAR(client)
	var oversized []string
	for letter := 'A'; len(oversized) <= MaxMETARStations; letter++ {
		oversized = append(oversized, "KAA"+string(letter))
	}
	for _, batch := range [][]string{{"KJFK", "LAX"}, {"@NY"}, {"KJFK,KLAX"}, oversized} {
		if _, err := feed.Observations(t.Context(), batch, time.Now()); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("query %v: %v", batch, err)
		}
	}
	if got, err := feed.Observations(t.Context(), nil, time.Now()); err != nil || len(got) != 0 {
		t.Fatalf("empty batch = %v, %v", got, err)
	}
}

func TestMETARBoundsResponsesAndDeclinesRedirects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "no data", status: http.StatusNoContent},
		{name: "empty array", status: http.StatusOK, body: `[]`},
		{name: "invalid", status: http.StatusOK, body: `null`, want: ErrUnavailable},
		{name: "trailing document", status: http.StatusOK, body: `[] []`, want: ErrUnavailable},
		{name: "oversized", status: http.StatusOK, body: strings.Repeat(" ", maxMETARBytes) + `[]`, want: ErrUnavailable},
		{name: "rate limited", status: http.StatusTooManyRequests, body: "ignored", want: ErrUnavailable},
		{name: "redirect", status: http.StatusTemporaryRedirect, want: ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					t.Fatalf("followed redirect to %s", r.URL)
				}
				res, err := response(tc.status, tc.body)
				res.Header = http.Header{"Location": {"https://untrusted.invalid/"}}
				return res, err
			})}
			got, err := NewMETAR(client).Observations(t.Context(), []string{"KJFK"}, time.Now())
			if len(got) != 0 || !errors.Is(err, tc.want) || calls != 1 {
				t.Fatalf("observations = %v, %v, requests = %d", got, err, calls)
			}
		})
	}
}
