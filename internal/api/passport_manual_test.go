package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestManualPassportFlightIsPrivateAndEditable(t *testing.T) {
	store := openPassportStore(t, filepath.Join(t.TempDir(), "manual-passport.db"))
	owner, ownerID := passportInstallation(t, store)
	other, _ := passportInstallation(t, store)
	provider := &passportSourceFixture{lookup: func(string) (feeds.Airport, error) {
		return feeds.Airport{}, feeds.ErrUnavailable
	}}
	handler := (Server{Store: store, Source: provider}).Handler()
	call := func(method, path, token string, input any, status int) []byte {
		t.Helper()
		var body []byte
		if input != nil {
			var err error
			body, err = json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
		}
		request := httptest.NewRequest(method, path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, response.Code, response.Body, status)
		}
		return response.Body.Bytes()
	}
	read := func(token string) passportPagedResponse {
		t.Helper()
		var view passportPagedResponse
		if err := json.Unmarshal(call(http.MethodGet, "/v1/passport?limit=10", token, nil, 200), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	const path = "/v1/passport/manual"
	flight := manualPassportInput{Date: "1999-06-02", Origin: " jfk ", Destination: "cdg", Number: "dl 42",
		TailNumber: "n123ab", AircraftModel: strings.Repeat("É", 50), DurationMinutes: new(90), ArrivalDelayMinutes: new(0),
		RequestID: "11111111-1111-4111-8111-111111111111"}
	call(http.MethodPost, path, "", flight, 401)
	invalid := flight
	invalid.Date = "2099-01-01"
	call(http.MethodPost, path, owner, invalid, 400)
	invalid = flight
	invalid.DurationMinutes = new(0)
	call(http.MethodPost, path, owner, invalid, 400)
	call(http.MethodPost, path, owner, flight, 201)
	replay := flight
	replay.TailNumber = "N99999"
	if got := call(http.MethodPost, path, owner, replay, 200); string(bytes.TrimSpace(got)) != `{"created":false}` {
		t.Fatalf("unexpected replay response %s", got)
	}
	first := read(owner)
	if len(first.Flights) != 1 || first.Stats.Flights != 1 || first.Stats.Hours != 1.5 || first.Stats.DelayMinutes != 0 ||
		first.Stats.Aircraft != 1 || first.Coverage.MissingDelayFlights != 0 || first.Coverage.MissingDurationFlights != 0 || len(first.Segments) != 1 {
		t.Fatalf("unexpected manual history %+v", first)
	}
	entry := first.Flights[0]
	if entry.Source != feeds.PassportManualSource || entry.Number != "DL42" || entry.TailNumber != "N123AB" || entry.AircraftModel != flight.AircraftModel ||
		entry.HistoryDate != flight.Date || entry.ActualDeparture != nil || entry.ActualArrival != nil {
		t.Fatalf("unexpected saved manual flight %+v", entry)
	}
	edit := flight
	edit.RequestID, edit.ExpectedObservedAt = "", &entry.ObservedAt
	edit.Date, edit.Destination = "2000-07-03", "LHR"
	edit.DurationMinutes, edit.ArrivalDelayMinutes = new(120), new(15)
	editPath := path + "/" + entry.ID
	call(http.MethodPut, editPath, other, edit, 404)
	call(http.MethodPut, editPath, owner, edit, 204)
	call(http.MethodPut, editPath, owner, edit, 204)
	stale := edit
	stale.Number = "DL43"
	call(http.MethodPut, editPath, owner, stale, 409)

	dateOnly := manualPassportInput{Date: "2002-01-01", Origin: "QZX", Destination: "ZZZ", RequestID: "22222222-2222-4222-8222-222222222222"}
	call(http.MethodPost, path, owner, dateOnly, 201)
	updated := read(owner)
	if len(updated.Flights) != 2 || updated.Flights[1].ID != entry.ID || updated.Flights[1].Destination != "LHR" ||
		updated.Flights[1].HistoryDate != edit.Date || !updated.Flights[1].ObservedAt.After(entry.ObservedAt) ||
		updated.Stats.Hours != 2 || updated.Stats.DelayMinutes != 15 || len(updated.Segments) != 1 ||
		updated.Coverage.MissingDurationFlights != 1 || updated.Coverage.MissingDelayFlights != 1 || updated.Coverage.MissingCoordinatesFlights != 1 {
		t.Fatalf("unexpected updated manual history %+v", updated)
	}
	if len(read(other).Flights) != 0 {
		t.Fatal("manual history leaked to another installation")
	}
	call(http.MethodGet, "/v1/flights/"+entry.ID, owner, nil, 404)
	deletePath := "/v1/passport/archive/" + entry.ID
	call(http.MethodDelete, deletePath, other, nil, 404)
	call(http.MethodDelete, deletePath, owner, nil, 204)
	if remaining := read(owner); len(remaining.Flights) != 1 || remaining.Flights[0].Origin != "QZX" {
		t.Fatalf("manual removal failed: %+v", remaining)
	}
	if flights, err := store.Tracked(t.Context(), ownerID); err != nil || len(flights) != 0 {
		t.Fatalf("manual history entered live tracking: %d flights, %v", len(flights), err)
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("manual history triggered %d airport-provider lookups", calls)
	}
}
