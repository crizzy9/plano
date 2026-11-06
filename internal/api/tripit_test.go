package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/itinerary"
)

type tripItTransport func(*http.Request) (*http.Response, error)

func (f tripItTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTripItRestrictsPrivateFeedOrigin(t *testing.T) {
	for _, raw := range []string{
		"https://tripit.com.evil.test/feed/ical/private/secret/tripit.ics",
		"https://www.tripit.com@evil.test/feed/ical/private/secret/tripit.ics",
		"https://www.tripit.com/feed/ical/%2e%2e/private/tripit.ics",
		"http://www.tripit.com/feed/ical/private/secret/tripit.ics",
		"https://www.tripit.com:443/feed/ical/private/secret/tripit.ics",
		"https://www.tripit.com/feed/ical/private/secret/tripit.ics?foo=bar",
	} {
		if _, err := tripItURL(raw); !errors.Is(err, errInvalid) {
			t.Fatalf("accepted %s", raw)
		}
	}
	target, err := tripItURL("webcal://tripit.com/feed/ical/private/sample-token/tripit.ics")
	if err != nil || target.Scheme != "https" || target.Host != "www.tripit.com" {
		t.Fatalf("target %v: %v", target, err)
	}
	client := &http.Client{Transport: tripItTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected request %v", r)
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"http://127.0.0.1/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}), CheckRedirect: tripItClient.CheckRedirect}
	if _, err := downloadTripIt(context.Background(), client, target); !errors.Is(err, feeds.ErrUnavailable) {
		t.Fatalf("redirect accepted: %v", err)
	}
}

func TestTripItPagesUpcomingFlightsAheadOfHistory(t *testing.T) {
	parsed := itinerary.Itinerary{}
	for _, day := range []string{"2020-01-01", "2020-02-01", "2020-03-01", "2020-04-01", "2020-05-01", "2020-06-01", "2020-07-01", "2020-08-01", "2020-09-01", "2027-01-07", "2027-01-05"} {
		parsed.Legs = append(parsed.Legs, itinerary.Leg{Number: "UA123", Date: day})
	}
	first := upcomingTripItPage(parsed, "2026-11-05", 0)
	if len(first.Legs) != 8 || first.Legs[0].Date != "2027-01-05" || first.Legs[1].Date != "2027-01-07" {
		t.Fatalf("first page %#v", first)
	}
	second := upcomingTripItPage(parsed, "2026-11-05", 8)
	if len(second.Legs) != 3 || second.Legs[2].Date != "2020-01-01" {
		t.Fatalf("second page %#v", second)
	}
}
