package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestPassportArchiveImportIsPrivateAndMapsWithoutProvider(t *testing.T) {
	store := openPassportStore(t, filepath.Join(t.TempDir(), "passport.db"))
	owner, ownerID := passportInstallation(t, store)
	other, _ := passportInstallation(t, store)
	provider := &passportSourceFixture{lookup: func(string) (feeds.Airport, error) {
		return feeds.Airport{}, feeds.ErrUnavailable
	}}
	handler := (Server{Store: store, Source: provider}).Handler()
	call := func(method, path, token, mime, body string, want int) string {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		request.Header.Set("Content-Type", mime)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, response.Code, response.Body, want)
		}
		for _, private := range []string{"fixture-booking", "fixture-private-note", "32A"} {
			if strings.Contains(response.Body.String(), private) {
				t.Fatal("Passport response exposed a private CSV field")
			}
		}
		return response.Body.String()
	}
	const importPath = "/v1/passport/import"
	const csv = `Date,Airline,Flight,From,To,Canceled,Gate Departure (Scheduled),Gate Departure (Actual),Gate Arrival (Actual),Gate Arrival (Scheduled),PNR,Seat,Notes
2019-06-02,DL,42,JFK,CDG,false,2019-06-02T19:55,2019-06-02T20:00,2019-06-03T10:00,2019-06-03T09:45,fixture-booking,32A,fixture-private-note
2018-05-03,,,QZX,ZZZ,false,,,,,,,
2018-05-03,UA,10,SFO,EWR,true,,,,,,,
`
	call(http.MethodPost, importPath, "", "text/csv", csv, 401)
	call(http.MethodPost, importPath, owner, "text/plain", csv, 415)
	call(http.MethodPost, importPath, owner, "text/csv", strings.Repeat("x", maxPassportCSVBytes+1), 413)
	call(http.MethodPost, importPath, owner, "text/csv", "wrong,header\na,b\n", 422)
	scheduleOnly := strings.Replace(csv, "2019-06-02T20:00,2019-06-03T10:00", ",", 1)
	for attempt, input := range []string{scheduleOnly, csv, csv} {
		want := `{"added":0,"duplicates":2,"skipped":1}`
		if attempt == 0 {
			want = `{"added":2,"duplicates":0,"skipped":1}`
		}
		if got := call(http.MethodPost, importPath, owner, "text/csv; charset=utf-8", input, 200); strings.TrimSpace(got) != want {
			t.Fatalf("attempt %d = %s, want %s", attempt, got, want)
		}
	}
	var first passportPagedResponse
	if err := json.Unmarshal([]byte(call(http.MethodGet, "/v1/passport?limit=1", owner, "", "", 200)), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Flights) != 1 || first.Flights[0].HistoryDate != "2019-06-02" || !strings.HasPrefix(first.Flights[0].HistoryKey, "a.") || first.NextCursor == "" ||
		first.Stats.Flights != 2 || first.Stats.Hours != 8 || first.Stats.DelayMinutes != 15 || len(first.Segments) != 1 || first.Stats.Miles < 3000 ||
		first.Coverage.MissingCoordinatesFlights != 1 || first.Coverage.MissingDurationFlights != 1 || first.Coverage.MissingDelayFlights != 1 {
		t.Fatalf("unexpected imported Passport summary: %+v", first)
	}
	var second passportHistoryPage
	if err := json.Unmarshal([]byte(call(http.MethodGet, "/v1/passport?limit=1&cursor="+url.QueryEscape(first.NextCursor), owner, "", "", 200)), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Flights) != 1 || second.Flights[0].HistoryDate != "2018-05-03" || second.Flights[0].ActualDeparture != nil || second.NextCursor != "" {
		t.Fatalf("unexpected imported Passport continuation: %+v", second)
	}
	if flights, err := store.Tracked(t.Context(), ownerID); err != nil || len(flights) != 0 {
		t.Fatalf("archive import wrote tracking rows: %d, %v", len(flights), err)
	}
	id := first.Flights[0].ID
	call(http.MethodGet, "/v1/flights/"+id, owner, "", "", 404)
	call(http.MethodDelete, "/v1/passport/archive/"+id, other, "", "", 404)
	call(http.MethodDelete, "/v1/passport/archive/"+id, owner, "", "", 204)
	var view passportResponse
	if err := json.Unmarshal([]byte(call(http.MethodGet, "/v1/passport", other, "", "", 200)), &view); err != nil || view.Stats.Flights != 0 {
		t.Fatalf("another installation saw archived history: %+v, %v", view, err)
	}
	if err := json.Unmarshal([]byte(call(http.MethodGet, "/v1/passport", owner, "", "", 200)), &view); err != nil || view.Stats.Flights != 1 {
		t.Fatalf("deleted archive remains in history: %+v, %v", view, err)
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("archive-only airport lookup reached the provider %d times", calls)
	}
}
