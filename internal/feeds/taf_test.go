package feeds

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTAFForecastsSelectLatestAvailableIssue(t *testing.T) {
	now := time.Date(2026, 11, 6, 17, 0, 0, 0, time.UTC)
	from, to := now.Add(-time.Hour).Unix(), now.Add(6*time.Hour).Unix()
	period := fmt.Sprintf(`"validTimeFrom":%d,"validTimeTo":%d`, from, to)
	thunderstorm := fmt.Sprintf(`[{"timeFrom":%d,"timeTo":%d,"wxString":"TSRA"}]`, from, to)
	clear := fmt.Sprintf(`[{"timeFrom":%d,"timeTo":%d,"visib":"6+","clouds":[{"cover":"NSC","base":null}]}]`, from, to)
	live := fmt.Sprintf(`[{"timeFrom":%d,"timeTo":%d,"visib":0.19,"wspd":28}]`, from, to)
	fixture := fmt.Sprintf(`[
		{"icaoId":"EGLL","issueTime":"2026-11-06T16:01:00Z",%s,"fcsts":%s},
		{"icaoId":"EGLL","issueTime":"2026-11-06T16:00:00Z",%s,"fcsts":%s},
		{"icaoId":"KJFK","issueTime":"2026-11-06T16:10:00Z","dbPopTime":"2026-11-06T16:10:58Z",%s,"fcsts":%s},
		{"icaoId":"KJFK","issueTime":"2026-11-06T16:10:00Z","dbPopTime":"2026-11-06T16:10:02Z",%s,"fcsts":%s},
		{"icaoId":"KJFK","issueTime":"2026-11-06T16:50:00Z","dbPopTime":"2026-11-06T17:00:01Z",%s,"fcsts":%s},
		{"icaoId":"KJFK","issueTime":"2026-11-06T17:01:00Z",%s,"fcsts":%s},
		{"icaoId":"KLAX","issueTime":"2026-11-06T16:00:00Z",%s,"fcsts":%s},
		{"icaoId":"KLAX","issueTime":"2026-11-06 16:45:00.000Z","rawTAF":"TAF AMD KLAX 061645Z 0616/0724 CNL=",%s,"fcsts":%s},
		{"icaoId":"KSFO","issueTime":"2026-11-06T16:00:00Z",%s,"fcsts":%s}
	]`, period, clear, period, thunderstorm, period, live, period, thunderstorm, period, thunderstorm,
		period, thunderstorm, period, thunderstorm, period, thunderstorm, period, thunderstorm)
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		query := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "aviationweather.gov" || r.URL.Path != "/api/data/taf" ||
			query.Get("format") != "json" || query.Get("time") != "issue" || query.Get("ids") != "EGLL,KJFK,KLAX" || !strings.HasPrefix(r.UserAgent(), "Plano/") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		return response(http.StatusOK, fixture)
	})}
	result, err := NewTAF(client).Forecasts(t.Context(), []string{"KJFK", " egll ", "KLAX", "EGLL"}, now)
	if err != nil || len(result) != 1 {
		t.Fatalf("forecasts = %#v, %v", result, err)
	}
	want := TAFPeriod{Airport: "KJFK", Conditions: "low visibility at 0.19 statute miles, sustained winds at 28 kt", Qualifier: "prevailing",
		IssuedAt: now.Add(-50 * time.Minute), StartsAt: time.Unix(from, 0).UTC(), EndsAt: time.Unix(to, 0).UTC()}
	if result[0] != want {
		t.Fatalf("forecast = %#v, want %#v", result[0], want)
	}
}

func TestTAFForecastsPreserveChangeGroupSemantics(t *testing.T) {
	now := time.Date(2026, 11, 6, 17, 0, 0, 0, time.UTC)
	at := func(hour int) int64 { return now.Add(time.Duration(hour) * time.Hour).Unix() }
	fixture := fmt.Sprintf(`[
		{"icaoId":"EDDF","issueTime":"2026-11-06T16:00:00Z","validTimeFrom":%d,"validTimeTo":%d,"fcsts":[
			{"timeFrom":%d,"timeTo":%d,"timeBec":%d,"fcstChange":"BECMG","visib":"6+","wspd":7,"clouds":[{"cover":"NSC","base":null}]},
			{"timeFrom":%d,"timeTo":%d,"timeBec":%d,"fcstChange":"BECMG","visib":1.86,"wspd":7,"wxString":"BR","clouds":[{"cover":"SCT","base":2000}]},
			{"timeFrom":%d,"timeTo":%d,"fcstChange":"TEMPO","probability":30,"wxString":"TSRA"},
			{"timeFrom":%d,"timeTo":%d,"fcstChange":"PROB","probability":40,"vertVis":100},
			{"timeFrom":%d,"timeTo":%d,"fcstChange":"TEMPO","wgst":39},
			{"timeFrom":%d,"timeTo":%d,"timeBec":null,"fcstChange":null,"visib":"6+","wspd":7},
			{"timeFrom":%d,"timeTo":%d,"fcstChange":"FM","wshearHgt":1500},
			{"timeFrom":%d,"timeTo":%d,"fcstChange":"PROB","wxString":"TSRA"},
			{"timeFrom":%d,"timeTo":%d,"fcstChange":"UNKNOWN","wxString":"TSRA"},
			{"timeFrom":%d,"timeTo":%d,"wxString":"TSRA"}
		]}
	]`, at(-1), at(8), at(5), at(7), at(6), at(1), at(5), at(2), at(2), at(3), at(3), at(4),
		at(4), at(5), at(-1), at(1), at(7), at(9), at(6), at(7), at(6), at(7), at(-1), at(0))
	client := &http.Client{Transport: testTransport(func(_ *http.Request) (*http.Response, error) {
		return response(http.StatusOK, fixture)
	})}
	got, err := NewTAF(client).Forecasts(t.Context(), []string{"EDDF"}, now)
	want := []struct {
		from, to    int
		condition   string
		qualifier   string
		probability int
	}{
		{1, 2, "low visibility at 1.86 statute miles", "becoming", 0},
		{2, 5, "low visibility at 1.86 statute miles", "prevailing", 0},
		{2, 3, "thunderstorms", "temporary", 30},
		{3, 4, "a low ceiling at 100 ft", "probability", 40},
		{4, 5, "gusts up to 39 kt", "temporary", 0},
		{5, 6, "low visibility at 1.86 statute miles may persist during the transition", "becoming", 0},
		{7, 8, "low-level wind shear", "prevailing", 0},
	}
	if err != nil || len(got) != len(want) {
		t.Fatalf("forecasts = %#v, %v", got, err)
	}
	for i, period := range got {
		w := want[i]
		if period.Airport != "EDDF" || period.IssuedAt != now.Add(-time.Hour) || period.StartsAt.Unix() != at(w.from) || period.EndsAt.Unix() != at(w.to) ||
			period.Conditions != w.condition || period.Qualifier != w.qualifier || period.Probability != w.probability {
			t.Errorf("forecast %d = %#v, want %+v", i, period, w)
		}
	}
}

func TestTAFBoundsQueriesAndResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "empty", status: http.StatusNoContent},
		{name: "invalid", status: http.StatusOK, body: `null`, want: ErrUnavailable},
		{name: "oversized", status: http.StatusOK, body: strings.Repeat(" ", maxTAFBytes) + `[]`, want: ErrUnavailable},
		{name: "redirect", status: http.StatusTemporaryRedirect, want: ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				res, err := response(tc.status, tc.body)
				res.Header = http.Header{"Location": {"https://untrusted.invalid/"}}
				return res, err
			})}
			feed := NewTAF(client)
			got, err := feed.Forecasts(t.Context(), []string{"KJFK"}, time.Now())
			if len(got) != 0 || !errors.Is(err, tc.want) || calls != 1 {
				t.Fatalf("forecasts = %v, %v, requests = %d", got, err, calls)
			}
			if _, err := feed.Forecasts(t.Context(), []string{"KJFK,KLAX"}, time.Now()); !errors.Is(err, ErrInvalidQuery) || calls != 1 {
				t.Fatalf("invalid query error = %v, requests = %d", err, calls)
			}
		})
	}
}
