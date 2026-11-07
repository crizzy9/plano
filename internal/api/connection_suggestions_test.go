package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

const suggestionsPath = "/v1/connection-suggestions"

func TestConnectionSuggestionsArePrivateAndReadOnly(t *testing.T) {
	service := newImportTestServer(t)
	service.Source = nil
	service.BookingKey = bytes.Repeat([]byte("k"), 32)
	handler := service.Handler()
	token, owner := newImportTestInstallation(t, service.Store)
	otherToken, other := newImportTestInstallation(t, service.Store)
	partialToken, partial := newImportTestInstallation(t, service.Store)
	now := time.Now().UTC().Truncate(time.Second)
	arrival, early, alternate, late := now.Add(time.Hour), now.Add(2*time.Hour), now.Add(10*time.Hour), now.Add(11*time.Hour)
	in := feeds.Flight{ID: "booked-in", Number: "PL1", Origin: "SFO", Destination: "DEN", Status: "scheduled", ScheduledArrival: &arrival, ObservedAt: now}
	out := feeds.Flight{ID: "booked-out", Number: "PL2", Origin: "DEN", OriginTimeZone: "America/Denver", Destination: "EWR", Status: "scheduled", ScheduledDeparture: &late, ObservedAt: now}
	earlyIn := feeds.Flight{ID: "early-in", Number: "PL3", Origin: "SFO", Destination: "SLC", Status: "scheduled", ScheduledArrival: &arrival, ObservedAt: now}
	earlyOut := feeds.Flight{ID: "early-out", Number: "PL4", Origin: "SLC", Destination: "EWR", Status: "scheduled", ScheduledDeparture: &early, ObservedAt: now}
	altIn := feeds.Flight{ID: "alt-in", Number: "PL5", Origin: "SFO", Destination: "DEN", Status: "scheduled", ScheduledArrival: &alternate, ObservedAt: now}
	for _, f := range []feeds.Flight{in, out, earlyIn, earlyOut, altIn} {
		observeSuggestionFlight(t, service.Store, f)
		trackSuggestionFlight(t, service.Store, other, f.ID)
	}
	for _, f := range []feeds.Flight{earlyIn, earlyOut, altIn} {
		trackSuggestionFlight(t, service.Store, owner, f.ID)
	}
	trackSuggestionFlight(t, service.Store, partial, in.ID)

	const locator = "ABC123"
	digest := service.bookingDigest(owner, locator)
	if _, err := service.Store.CommitImport(t.Context(), owner, []feeds.Flight{in, out}, digest, "", ""); err != nil {
		t.Fatal(err)
	}
	// A digest can only participate in a pair inside its own installation.
	if _, err := service.Store.CommitImport(t.Context(), other, []feeds.Flight{out}, digest, "", ""); err != nil {
		t.Fatal(err)
	}
	list := func(token string) []connectionSuggestion {
		t.Helper()
		body := importTestRequest(t, handler, http.MethodGet, suggestionsPath, token, "", http.StatusOK)
		if strings.Contains(body, locator) || strings.Contains(body, digest) || strings.Contains(body, "locator") {
			t.Fatal("suggestions expose the private booking index")
		}
		var decoded struct{ Suggestions []connectionSuggestion }
		if err := json.Unmarshal([]byte(body), &decoded); err != nil || decoded.Suggestions == nil {
			t.Fatalf("invalid suggestions response %s, %v", body, err)
		}
		return decoded.Suggestions
	}

	importTestRequest(t, handler, http.MethodGet, suggestionsPath, "", "", http.StatusUnauthorized)
	importTestRequest(t, handler, http.MethodPost, suggestionsPath, token, "", http.StatusMethodNotAllowed)
	got := list(token)
	if len(got) != 3 || got[0].Inbound != in.ID || got[0].Outbound != out.ID || got[0].Source != "booking" ||
		got[1].Outbound != earlyOut.ID || got[1].Source != "schedule" || got[2].Inbound != altIn.ID || got[2].Outbound != out.ID {
		t.Fatalf("unexpected suggestion order: %+v", got)
	}
	booked := got[0]
	if booked.InboundNumber != in.Number || booked.OutboundNumber != out.Number || booked.Airport != "DEN" ||
		booked.TimeZone != out.OriginTimeZone || !booked.ScheduledArrival.Equal(arrival) ||
		!booked.ScheduledDeparture.Equal(late) || booked.ScheduledMinutes != 600 {
		t.Fatalf("unexpected connection summary: %+v", booked)
	}
	if other := list(otherToken); len(other) != 2 || other[0].Source != "schedule" || other[1].Source != "schedule" {
		t.Fatalf("another installation received private booking evidence: %+v", other)
	}
	if partial := list(partialToken); len(partial) != 0 {
		t.Fatalf("received an untracked leg: %+v", partial)
	}
	if monitors, err := service.Store.ConnectionMonitors(t.Context(), owner); err != nil || len(monitors) != 0 {
		t.Fatalf("suggestion read changed monitors: %+v, %v", monitors, err)
	}

	importTestRequest(t, handler, http.MethodPut, "/v1/connection-monitors?inbound="+in.ID+"&outbound="+out.ID, token, "", http.StatusOK)
	if got := list(token); len(got) != 2 || got[1].Inbound != altIn.ID || got[1].Outbound != out.ID {
		t.Fatalf("monitor should hide only its exact pair: %+v", got)
	}
	for _, f := range []feeds.Flight{in, earlyIn, altIn} {
		if err := service.Store.Untrack(t.Context(), owner, f.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := list(token); len(got) != 0 {
		t.Fatalf("untracked flights were suggested: %+v", got)
	}
}

func TestConnectionSuggestionEligibility(t *testing.T) {
	service := newImportTestServer(t)
	now := time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC)
	latest := now.Add(connectionSuggestionHorizon)
	for _, test := range []struct {
		name        string
		departure   time.Duration
		gap         time.Duration
		booked      bool
		want        string
		wantAirport string
		change      func(in, out *feeds.Flight)
	}{
		{name: "schedule window boundary", departure: time.Hour, gap: 8 * time.Hour, want: "schedule"},
		{name: "outside schedule window", departure: time.Hour, gap: 8*time.Hour + time.Minute},
		{name: "booking window boundary", departure: time.Hour, gap: 24 * time.Hour, booked: true, want: "booking"},
		{name: "outside booking window", departure: time.Hour, gap: 24*time.Hour + time.Minute, booked: true},
		{name: "zero window", departure: time.Hour, booked: true},
		{name: "reversed schedule", departure: time.Hour, gap: -time.Minute, booked: true},
		{name: "live inversion", departure: time.Hour, gap: time.Minute, want: "schedule", change: func(in, out *feeds.Flight) {
			late := out.ScheduledDeparture.Add(time.Hour)
			in.EstimatedArrival = &late
		}},
		{name: "past departure", departure: -time.Minute, gap: time.Hour},
		{name: "effective departure now", departure: 0, gap: time.Hour},
		{name: "delayed future departure", departure: -48 * time.Hour, gap: time.Hour, want: "schedule", change: func(_, out *feeds.Flight) {
			delayed := now.Add(time.Hour)
			out.EstimatedDeparture = &delayed
		}},
		{name: "horizon boundary", departure: connectionSuggestionHorizon, gap: time.Hour, want: "schedule"},
		{name: "outside horizon", departure: connectionSuggestionHorizon + time.Minute, gap: time.Hour},
		{name: "canceled inbound", departure: time.Hour, gap: time.Hour, booked: true, change: func(in, _ *feeds.Flight) {
			in.Status = "cancelled"
		}},
		{name: "diverted inbound", departure: time.Hour, gap: time.Hour, booked: true, change: func(in, _ *feeds.Flight) {
			in.Status = "diverted"
		}},
		{name: "canceled onward", departure: time.Hour, gap: time.Hour, booked: true, change: func(_, out *feeds.Flight) {
			out.Status = "cancelled"
		}},
		{name: "departed onward", departure: time.Hour, gap: time.Hour, booked: true, change: func(_, out *feeds.Flight) {
			out.ActualTakeoff = &now
		}},
		{name: "verified airports conflict", departure: time.Hour, gap: time.Hour, booked: true, change: func(in, out *feeds.Flight) {
			in.DestinationICAO, out.OriginICAO = "KDEN", "KBJC"
		}},
		{name: "matching verified airports", departure: time.Hour, gap: time.Hour, want: "schedule", wantAirport: "KDEN", change: func(in, out *feeds.Flight) {
			in.Destination, out.Origin = "", ""
			in.DestinationICAO, out.OriginICAO = "KDEN", "KDEN"
		}},
		{name: "matching ICAO with conflicting display codes", departure: time.Hour, gap: time.Hour, want: "schedule", wantAirport: "KDEN", change: func(in, out *feeds.Flight) {
			in.Destination, out.Origin = "BJC", "DEN"
			in.DestinationICAO, out.OriginICAO = "KDEN", "KDEN"
		}},
		{name: "matching display codes identify another airport", departure: time.Hour, gap: time.Hour, want: "schedule", wantAirport: "KDEN", change: func(in, out *feeds.Flight) {
			in.Destination, out.Origin = "LAX", "LAX"
			in.DestinationICAO, out.OriginICAO = "KDEN", "KDEN"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, owner := newImportTestInstallation(t, service.Store)
			departure := now.Add(test.departure)
			arrival := departure.Add(-test.gap)
			in := feeds.Flight{ID: "in-" + strings.ReplaceAll(test.name, " ", "-"), Number: "PL1", Destination: "DEN", Status: "scheduled", ScheduledArrival: &arrival, ObservedAt: now}
			out := feeds.Flight{ID: "out-" + strings.ReplaceAll(test.name, " ", "-"), Number: "PL2", Origin: "DEN", Status: "scheduled", ScheduledDeparture: &departure, ObservedAt: now}
			if test.change != nil {
				test.change(&in, &out)
			}
			for _, f := range []feeds.Flight{in, out} {
				observeSuggestionFlight(t, service.Store, f)
				trackSuggestionFlight(t, service.Store, owner, f.ID)
			}
			if test.booked {
				if _, err := service.Store.CommitImport(t.Context(), owner, []feeds.Flight{in, out}, "shared-test-digest", "", ""); err != nil {
					t.Fatal(err)
				}
			}
			candidates, err := service.Store.ConnectionSuggestionFlights(t.Context(), owner, now, latest)
			if err != nil {
				t.Fatal(err)
			}
			got := rankConnectionSuggestions(candidates, now, latest)
			if test.want == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected connection: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Inbound != in.ID || got[0].Outbound != out.ID || got[0].Source != test.want {
				t.Fatalf("expected %s connection, got %+v", test.want, got)
			}
			if test.wantAirport != "" && got[0].Airport != test.wantAirport {
				t.Fatalf("got airport %q, want %q", got[0].Airport, test.wantAirport)
			}
		})
	}
}

func TestConnectionSuggestionLimitsAndStableOrder(t *testing.T) {
	now := time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC)
	arrival, departure := now.Add(time.Hour), now.Add(2*time.Hour)
	var candidates []core.ConnectionSuggestionFlight
	for i := 4; i >= 0; i-- {
		candidates = append(candidates, core.ConnectionSuggestionFlight{Flight: feeds.Flight{
			ID: fmt.Sprintf("in-%d", i), Destination: "DEN", ScheduledArrival: &arrival,
		}}, core.ConnectionSuggestionFlight{Flight: feeds.Flight{
			ID: fmt.Sprintf("out-%d", i), Origin: "DEN", ScheduledDeparture: &departure,
		}})
	}
	got := rankConnectionSuggestions(candidates, now, now.Add(connectionSuggestionHorizon))
	if len(got) != 8 {
		t.Fatalf("expected eight results, got %d", len(got))
	}
	for i, suggestion := range got {
		wantIn, wantOut := fmt.Sprintf("in-%d", i/5), fmt.Sprintf("out-%d", i%5)
		if suggestion.Inbound != wantIn || suggestion.Outbound != wantOut {
			t.Fatalf("result %d is %s/%s, want %s/%s", i, suggestion.Inbound, suggestion.Outbound, wantIn, wantOut)
		}
	}
}

func observeSuggestionFlight(t *testing.T, store *core.Store, f feeds.Flight) {
	t.Helper()
	if err := store.Observe(t.Context(), f, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func trackSuggestionFlight(t *testing.T, store *core.Store, owner, id string) {
	t.Helper()
	if err := store.Track(t.Context(), owner, id, ""); err != nil {
		t.Fatal(err)
	}
}
