package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestPassportExactOverlapRetainsPrivateArchiveMeasurements(t *testing.T) {
	store := openPassportStore(t, filepath.Join(t.TempDir(), "passport-overlap.db"))
	token, owner := passportInstallation(t, store)
	otherToken, other := passportInstallation(t, store)
	handler := (Server{Store: store}).Handler()
	const archive = `Date,Airline,Flight,From,To,Gate Departure (Scheduled),Gate Departure (Actual),Gate Arrival (Scheduled),Gate Arrival (Actual),Tail Number
2024-06-02,UA,123,SFO,EWR,2024-06-02T10:00,2024-06-02T10:10,2024-06-02T18:20,2024-06-02T18:40,N123AA
`
	request := httptest.NewRequest(http.MethodPost, "/v1/passport/import", strings.NewReader(archive))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "text/csv")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"added":1`) {
		t.Fatalf("import status %d: %s", response.Code, response.Body)
	}
	scheduled := time.Date(2024, 6, 2, 17, 0, 0, 0, time.UTC)
	flight := feeds.Flight{ID: "UAL123-passport-overlap", Number: "UA123", ICAONumber: "UAL123", Source: "flightaware_aeroapi",
		Origin: "SFO", OriginICAO: "KSFO", Destination: "EWR", DestinationICAO: "KEWR",
		Status: "arrived", ScheduledDeparture: &scheduled, ObservedAt: scheduled.Add(8 * time.Hour)}
	addPassportFlight(t, store, owner, flight)
	if err := store.Track(t.Context(), other, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	read := func(path, credential string) passportPagedResponse {
		t.Helper()
		body := importTestRequest(t, handler, http.MethodGet, path, credential, "", http.StatusOK)
		var got passportPagedResponse
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, path := range []string{"/v1/passport", "/v1/passport?limit=1"} {
		got := read(path, token)
		if len(got.Flights) != 1 || got.Flights[0].ID != flight.ID || got.Flights[0].Source != flight.Source || got.NextCursor != "" ||
			got.Stats.Flights != 1 || got.Stats.Hours != 5.5 || got.Stats.DelayMinutes != 20 || got.Stats.Aircraft != 1 ||
			got.Stats.Miles < 2000 || len(got.Segments) != 1 || got.Coverage.MissingCoordinatesFlights != 0 ||
			got.Coverage.MissingDurationFlights != 0 || got.Coverage.MissingDelayFlights != 0 {
			t.Fatalf("%s lost measurements or tracked identity: %+v", path, got)
		}
		if strings.Contains(path, "?") && got.Flights[0].HistoryKey != "p."+flight.ID {
			t.Fatalf("tracked history key = %q", got.Flights[0].HistoryKey)
		}
	}
	otherHistory := read("/v1/passport?limit=1", otherToken)
	if len(otherHistory.Flights) != 1 || otherHistory.Stats.Hours != 0 || otherHistory.Stats.DelayMinutes != 0 || otherHistory.Stats.Aircraft != 0 ||
		otherHistory.Flights[0].HistoryDate != "" ||
		otherHistory.Flights[0].ActualDeparture != nil || otherHistory.Flights[0].ActualArrival != nil {
		t.Fatalf("other installation received private archive facts: %+v", otherHistory)
	}
	if saved, err := store.Flight(t.Context(), flight.ID); err != nil || saved.ActualDeparture != nil || saved.ActualArrival != nil ||
		saved.TailNumber != "" || saved.HistoryDate != "" {
		t.Fatalf("archive facts reached the shared flight cache: %+v, %v", saved, err)
	}
	importTestRequest(t, handler, http.MethodDelete, "/v1/tracked/"+flight.ID, token, "", http.StatusNoContent)
	restored := read("/v1/passport?limit=1", token)
	if len(restored.Flights) != 1 || restored.Flights[0].Source != "flighty_csv" || restored.Stats.Hours != 5.5 || restored.Stats.DelayMinutes != 20 {
		t.Fatalf("archive row did not return after untracking: %+v", restored)
	}
}

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
