package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestImportedBookingSuggestsPrivateConnectionWithoutMonitoring(t *testing.T) {
	service := newImportTestServer(t)
	service.BookingKey = bytes.Repeat([]byte("k"), 32)
	handler := service.Handler()
	token, owner := newImportTestInstallation(t, service.Store)
	otherToken, otherOwner := newImportTestInstallation(t, service.Store)

	day := time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly)
	const booking = "ZXC12B"
	payload, err := json.Marshal(map[string]string{
		"source": "email",
		"text":   "Booking reference: " + booking + "\nUA123 " + day + "\nUA456 " + day,
	})
	if err != nil {
		t.Fatal(err)
	}
	importTestRequest(t, handler, http.MethodPost, "/v1/import", token, string(payload), http.StatusOK)

	inbound, outbound := "demo-UA123-"+day, "demo-UA456-"+day
	const path = "/v1/connection-suggestions"
	assertSuggestions := func(token, source string) {
		t.Helper()
		body := importTestRequest(t, handler, http.MethodGet, path, token, "", http.StatusOK)
		if strings.Contains(body, booking) || strings.Contains(body, service.bookingDigest(owner, booking)) {
			t.Fatal("suggestion exposed private booking data")
		}
		var result struct {
			Suggestions []struct{ Inbound, Outbound, Source string }
		}
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			t.Fatal(err)
		}
		if source == "" {
			if len(result.Suggestions) != 0 {
				t.Fatalf("suggestions = %v, want none", result.Suggestions)
			}
			return
		}
		if len(result.Suggestions) != 1 {
			t.Fatalf("suggestions = %v, want one %s match", result.Suggestions, source)
		}
		got := result.Suggestions[0]
		if got.Inbound != inbound || got.Outbound != outbound || got.Source != source {
			t.Fatalf("suggestion = %+v, want %s to %s with source %s", got, inbound, outbound, source)
		}
	}

	assertSuggestions(token, "booking")
	assertSuggestions(otherToken, "")
	for _, id := range []string{inbound, outbound} {
		if err := service.Store.Track(t.Context(), otherOwner, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	assertSuggestions(otherToken, "schedule")
	monitors, err := service.Store.ConnectionMonitors(t.Context(), owner)
	if err != nil || len(monitors) != 0 {
		t.Fatalf("suggesting a connection created monitoring: %v, %v", monitors, err)
	}

	pair := url.Values{"inbound": {inbound}, "outbound": {outbound}}.Encode()
	assessment := importTestRequest(t, handler, http.MethodGet, "/v1/connections?"+pair, token, "", http.StatusOK)
	var connection struct{ Monitoring, MonitorEligible bool }
	if err := json.Unmarshal([]byte(assessment), &connection); err != nil || connection.Monitoring || !connection.MonitorEligible {
		t.Fatalf("unexpected assessment for suggestion: %s, %v", assessment, err)
	}
	importTestRequest(t, handler, http.MethodPut, "/v1/connection-monitors?"+pair, token, "", http.StatusOK)
	assertSuggestions(token, "")
	assertSuggestions(otherToken, "schedule")
}
