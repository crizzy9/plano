package feeds

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

const reservationFixtureEndpoint = "https://fixture.invalid/v1/reservations/lookup"

func reservationResponse(status int, kind, body string) (*http.Response, error) {
	reply, err := response(status, body)
	reply.Header = http.Header{"Content-Type": []string{kind}}
	return reply, err
}

func TestReservationGatewayUsesFixedAuthorizedPOST(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(reservationFixtureEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(endpoint, []*http.Cookie{{Name: "unrelated", Value: "fixture-cookie"}})
	var calls int
	client := &http.Client{Timeout: time.Minute, Jar: jar, Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.String() != reservationFixtureEndpoint {
			t.Error("unexpected reservation request method or endpoint")
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("Cookie") != "" ||
			r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" ||
			r.Header.Get("Cache-Control") != "no-store" {
			t.Error("reservation request headers do not match the contract")
		}
		var request struct {
			Version int
			ReservationQuery
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil ||
			request.Version != 1 || request.ReservationQuery != (ReservationQuery{"UA", "ZX1234", "D’Ávila-Li"}) {
			t.Error("request did not normalize only carrier and locator")
		}
		return reservationResponse(http.StatusOK, "application/json; charset=utf-8", `{
		  "version": 1, "authorized": true, "legs": [
		    {"number":"UA7","date":"2026-11-05"},
		    {"number":"UA7","date":"2026-11-05"},
		    {"number":"UAL8A","date":"2026-11-06"}
		  ]
		}`)
	})}
	gateway, err := NewReservationGateway(reservationFixtureEndpoint, "fixture-token", client)
	if err != nil {
		t.Fatal(err)
	}
	if gateway.client.Timeout != maxReservationRequestTime || client.Timeout != time.Minute || client.Jar != jar {
		t.Fatal("gateway changed the supplied client or did not bound its own timeout")
	}
	got, err := gateway.Lookup(t.Context(), ReservationQuery{" ua ", "zx1234", " D’Ávila-Li "})
	if err != nil || calls != 1 || !slices.Equal(got, []ReservationLeg{{"UA7", "2026-11-05"}, {"UAL8A", "2026-11-06"}}) {
		t.Fatalf("unexpected reservation result %+v, calls=%d, err=%v", got, calls, err)
	}
}

func TestReservationGatewayRejectsInvalidConfigurationAndQueries(t *testing.T) {
	for _, endpoint := range []string{
		"http://fixture.invalid/lookup", "http://localhost/lookup", "https://alice:secret@fixture.invalid/lookup",
		"https://fixture.invalid/lookup?token=secret", "https://fixture.invalid/lookup?", "https://fixture.invalid/lookup#private",
	} {
		if _, err := NewReservationGateway(endpoint, "fixture-token", nil); !errors.Is(err, ErrInvalidQuery) {
			t.Fatal("accepted invalid gateway endpoint")
		}
	}
	for _, token := range []string{"", "line\r\nheader", "contains a space", strings.Repeat("A", 4097)} {
		if _, err := NewReservationGateway(reservationFixtureEndpoint, token, nil); !errors.Is(err, ErrInvalidQuery) {
			t.Fatal("accepted invalid gateway credential")
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:8080/lookup", "http://[::1]:8080/lookup"} {
		if _, err := NewReservationGateway(endpoint, "fixture-token", nil); err != nil {
			t.Fatal(err)
		}
	}
	client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		t.Error("invalid reservation query reached the transport")
		return nil, errors.New("unexpected request")
	})}
	gateway, err := NewReservationGateway(reservationFixtureEndpoint, "fixture-token", client)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []ReservationQuery{
		{"", "ZX1234", "Fixture"}, {"U4X", "ZX1234", "Fixture"}, {"UA", "ZX123", "Fixture"},
		{"UA", "ZX1234", ""}, {"UA", "ZX1234", "Family\nName"}, {"UA", "ZX1234", "Fami\u202ely"},
		{"UA", "ZX1234", "1234"}, {"UA", "ZX1234", strings.Repeat("A", 81)},
	} {
		if _, err := gateway.Lookup(t.Context(), query); !errors.Is(err, ErrInvalidQuery) {
			t.Fatal("invalid reservation query did not fail validation")
		}
	}
}

func TestReservationGatewayRejectsRedirectAndSanitizesTransportFailure(t *testing.T) {
	for _, redirect := range []bool{true, false} {
		var calls int
		client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
			calls++
			if !redirect {
				return nil, errors.New("raw transport error with ZX1234 and Fixture")
			}
			reply, err := reservationResponse(http.StatusTemporaryRedirect, "text/plain", "ZX1234 Fixture")
			reply.Header.Set("Location", "https://elsewhere.invalid/lookup")
			return reply, err
		})}
		gateway, err := NewReservationGateway(reservationFixtureEndpoint, "fixture-token", client)
		if err != nil {
			t.Fatal(err)
		}
		legs, err := gateway.Lookup(t.Context(), ReservationQuery{"UA", "ZX1234", "Fixture"})
		if calls != 1 || legs != nil || !errors.Is(err, ErrUnavailable) ||
			strings.Contains(err.Error(), "ZX1234") || strings.Contains(err.Error(), "Fixture") {
			t.Fatal("reservation request followed a redirect or exposed private data")
		}
	}
}

func TestReservationGatewayRejectsUnverifiedOrMalformedResults(t *testing.T) {
	const validLeg = `{"number":"UA7","date":"2026-11-05"}`
	tooMany := strings.Repeat(validLeg+",", maxReservationLegs) + validLeg
	cases := []struct {
		name, body, kind string
		status           int
		want             error
	}{
		{"absent or mismatched", "raw ZX1234 Fixture", "text/plain", http.StatusNotFound, ErrNotFound},
		{"bridge authorization failure", "raw ZX1234 Fixture", "text/plain", http.StatusForbidden, ErrUnavailable},
		{"bridge input failure", "raw ZX1234 Fixture", "text/plain", http.StatusBadRequest, ErrUnavailable},
		{"not verified", `{"version":1,"authorized":false,"legs":[]}`, "application/json", http.StatusOK, ErrNotFound},
		{"no flights", `{"version":1,"authorized":true,"legs":[]}`, "application/json", http.StatusOK, ErrNotFound},
		{"missing verification", `{"version":1,"legs":[` + validLeg + `]}`, "application/json", http.StatusOK, ErrUnavailable},
		{"private extra field", `{"version":1,"authorized":true,"legs":[{"number":"UA7","date":"2026-11-05","passenger":"Fixture"}]}`, "application/json", http.StatusOK, ErrUnavailable},
		{"locator masquerading as flight", `{"version":1,"authorized":true,"legs":[{"number":"ZX1234","date":"2026-11-05"}]}`, "application/json", http.StatusOK, ErrUnavailable},
		{"not a local date", `{"version":1,"authorized":true,"legs":[{"number":"UA7","date":"2026-02-29"}]}`, "application/json", http.StatusOK, ErrUnavailable},
		{"un-normalized designator", `{"version":1,"authorized":true,"legs":[{"number":"UA 7","date":"2026-11-05"}]}`, "application/json", http.StatusOK, ErrUnavailable},
		{"extra JSON value", `{"version":1,"authorized":true,"legs":[` + validLeg + `]} {}`, "application/json", http.StatusOK, ErrUnavailable},
		{"too many flights", `{"version":1,"authorized":true,"legs":[` + tooMany + `]}`, "application/json", http.StatusOK, ErrUnavailable},
		{"oversized body", strings.Repeat(" ", maxReservationResponseBytes+1), "application/json", http.StatusOK, ErrUnavailable},
		{"wrong content type", `{"version":1,"authorized":true,"legs":[` + validLeg + `]}`, "text/plain", http.StatusOK, ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
				return reservationResponse(tc.status, tc.kind, tc.body)
			})}
			gateway, err := NewReservationGateway(reservationFixtureEndpoint, "fixture-token", client)
			if err != nil {
				t.Fatal(err)
			}
			legs, err := gateway.Lookup(t.Context(), ReservationQuery{"UA", "ZX1234", "Fixture"})
			if legs != nil || !errors.Is(err, tc.want) || strings.Contains(err.Error(), "ZX1234") || strings.Contains(err.Error(), "Fixture") {
				t.Fatal("gateway accepted unusable data or returned private information")
			}
		})
	}
}
