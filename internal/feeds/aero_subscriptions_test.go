package feeds

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAeroSubscriptionUsesDocumentedRouteDayAndReadOnlyEndpointCheck(t *testing.T) {
	const target = "https://plano.example.com/v1/webhooks/aeroapi?plano_alert=marker&token=fixture"
	scope := AeroAlertScope{Ident: "UAL7", Origin: "KSFO", Destination: "KJFK", Date: "2026-11-06"}
	var paths []string
	client := NewAeroAPI("fixture-api-key", &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("x-apikey") != "fixture-api-key" {
			t.Error("missing FlightAware API header")
		}
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "GET /aeroapi/alerts/endpoint":
			return response(200, `{"url":"https://unrelated-account-endpoint.example/alerts"}`)
		case "POST /aeroapi/alerts":
			var payload struct {
				Ident, Origin, Destination, Start, End string
				TargetURL                              string          `json:"target_url"`
				MaxWeekly                              int             `json:"max_weekly"`
				Events                                 map[string]bool `json:"events"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Ident != scope.Ident || payload.Origin != scope.Origin ||
				payload.Destination != scope.Destination || payload.Start != scope.Date || payload.End != scope.Date ||
				payload.TargetURL != target || payload.MaxWeekly != 64 || !payload.Events["on"] || !payload.Events["cancelled"] || payload.Events["filed"] {
				t.Errorf("unexpected create request shape: %v", err)
			}
			result, err := response(201, "")
			result.Header = http.Header{"Location": {"/aeroapi/alerts/101"}}
			return result, err
		case "GET /aeroapi/alerts":
			if r.URL.Query().Get("max_pages") != "1" {
				t.Error("inventory was not page-bounded")
			}
			if r.URL.Query().Get("cursor") == "next-2" {
				return response(200, `{"alerts":[],"links":{"next":null}}`)
			}
			return response(200, `{"alerts":[{"id":101,"ident":"UAL7"}],"links":{"next":"/aeroapi/alerts?cursor=next-2"}}`)
		case "GET /aeroapi/alerts/101":
			return response(200, `{"id":101,"ident":"UA7","user_ident":"UAL7","origin":"SFO","origin_icao":"KSFO","destination":"JFK","destination_icao":"KJFK","start":"2026-11-06","end":"2026-11-06","target_url":"`+target+`"}`)
		case "DELETE /aeroapi/alerts/101":
			return response(204, "")
		default:
			return nil, errors.New("unexpected provider fixture request")
		}
	})})
	ctx := t.Context()
	if configured, err := client.AeroAlertEndpointConfigured(ctx); err != nil || !configured {
		t.Fatalf("endpoint configured = %v, %v", configured, err)
	}
	if id, err := client.CreateAeroSubscription(ctx, scope, target); err != nil || id != 101 {
		t.Fatalf("create = %d, %v", id, err)
	}
	if rows, cursor, err := client.ListAeroSubscriptions(ctx, ""); err != nil || len(rows) != 1 || cursor != "next-2" {
		t.Fatalf("inventory = %d %q %v", len(rows), cursor, err)
	}
	if rows, cursor, err := client.ListAeroSubscriptions(ctx, "next-2"); err != nil || len(rows) != 0 || cursor != "" {
		t.Fatalf("inventory completion = %d %q %v", len(rows), cursor, err)
	}
	if got, err := client.InspectAeroSubscription(ctx, 101); err != nil || !got.MatchesScope(scope) || got.TargetURL != target {
		t.Fatalf("inspection invalid: %v", err)
	}
	if err := client.DeleteAeroSubscription(ctx, 101); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 6 {
		t.Fatalf("unexpected requests: %v", paths)
	}
}

func TestAeroSubscriptionRejectsForeignReferencesAndUnverifiedScope(t *testing.T) {
	scope := AeroAlertScope{Ident: "UAL7", Origin: "KSFO", Destination: "KJFK", Date: "2026-11-06"}
	const target = "https://plano.example.com/v1/webhooks/aeroapi"
	calls := 0
	client := NewAeroAPI("fixture-api-key", &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == http.MethodPost {
			result, err := response(201, "")
			result.Header = http.Header{"Location": {"https://unrelated.example/aeroapi/alerts/101"}}
			return result, err
		}
		return response(200, `{"alerts":[],"links":{"next":"https://unrelated.example/aeroapi/alerts?cursor=secret"}}`)
	})})
	if _, err := client.CreateAeroSubscription(t.Context(), scope, target); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "unrelated.example") {
		t.Fatalf("foreign Location not rejected safely: %v", err)
	}
	if _, _, err := client.ListAeroSubscriptions(t.Context(), ""); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("foreign cursor not rejected safely: %v", err)
	}
	scope.Ident = "UAL7-123456789-airline-0001"
	if _, err := client.CreateAeroSubscription(t.Context(), scope, target); !errors.Is(err, ErrInvalidQuery) || calls != 2 {
		t.Fatalf("unsupported exact ID generated a request: calls=%d error=%v", calls, err)
	}
}

func TestAeroAlertScopeUsesDepartureLocalDay(t *testing.T) {
	flight := Flight{Source: aeroSource, ICAONumber: "UAL7", OriginICAO: "KSFO", DestinationICAO: "KJFK", OriginTimeZone: "America/Los_Angeles",
		ScheduledDeparture: new(instant(t, "2026-11-06T05:50:00Z"))}
	got, starts, ok := flight.AeroAlertScope()
	if !ok || got.Date != "2026-11-05" || starts.Hour() != 0 || starts.Location().String() != flight.OriginTimeZone || !starts.Equal(time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("local scope = %+v, %v, %v", got, starts, ok)
	}
	flight.OriginTimeZone = ""
	if _, _, ok := flight.AeroAlertScope(); ok {
		t.Fatal("missing departure timezone guessed")
	}
}
