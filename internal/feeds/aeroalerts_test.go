package feeds

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeAeroAlertUsesOnlyDocumentedRoutingFields(t *testing.T) {
	body := `{
		"long_description":"Fixture flight information has changed",
		"short_description":"Fixture gate change",
		"summary":"Fixture change",
		"event_code":"change",
		"alert_id":2030000001,
		"flight":{
			"fa_flight_id":"UAL7-1783020101-airline-0001",
			"ident":"UAL7","ident_iata":"UA7","origin":"KSFO","destination":"KJFK",
			"gate_origin":"C12","scheduled_out":"2026-11-05T19:00:00Z","actual_out":null,
			"blocked":false,"cancelled":false,"diverted":false
		}
	}`
	got, err := DecodeAeroAlert(strings.NewReader(body))
	want := AeroAlert{SubscriptionID: 2030000001, FlightID: "UAL7-1783020101-airline-0001", EventCode: "change"}
	if err != nil || got != want {
		t.Fatalf("callback = %+v, %v, want %+v", got, err, want)
	}
	// A future provider event can still wake an authoritative refresh.
	body = strings.Replace(body, `"event_code":"change"`, `"event_code":"hold_start"`, 1)
	got, err = DecodeAeroAlert(strings.NewReader(body))
	if err != nil || got.EventCode != "hold_start" {
		t.Fatalf("additional event code = %+v, %v", got, err)
	}
}

func TestDecodeAeroAlertSuppressesBlockedAndUnavailableFlights(t *testing.T) {
	for _, body := range []string{
		`{"alert_id":12,"event_code":"out","flight":{"fa_flight_id":"GA-fixture","blocked":true}}`,
		`{"alert_id":12,"event_code":"out","flight":{"error":"Fixture provider could not assemble details"}}`,
	} {
		got, err := DecodeAeroAlert(strings.NewReader(body))
		if !errors.Is(err, ErrNotFound) || got != (AeroAlert{}) {
			t.Fatalf("unavailable flight = %+v, %v", got, err)
		}
	}
}

func TestDecodeAeroAlertRejectsMalformedOrOversizedCallbacks(t *testing.T) {
	for name, body := range map[string]string{
		"missing flight":     `{"alert_id":12,"event_code":"out"}`,
		"missing event":      `{"alert_id":12,"flight":{"fa_flight_id":"UAL7-fixture"}}`,
		"invalid event code": `{"alert_id":12,"event_code":"out?","flight":{"fa_flight_id":"UAL7-fixture"}}`,
		"nonpositive ID":     `{"alert_id":-1,"event_code":"out","flight":{"fa_flight_id":"UAL7-fixture"}}`,
		"ID traversal":       `{"alert_id":12,"event_code":"out","flight":{"fa_flight_id":"../keys"}}`,
		"missing ID":         `{"alert_id":12,"event_code":"out","flight":{}}`,
		"trailing object":    `{"alert_id":12,"event_code":"out","flight":{"fa_flight_id":"UAL7-fixture"}}{}`,
		"too large":          strings.Repeat(" ", MaxAeroAlertBytes) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeAeroAlert(strings.NewReader(body))
			if !errors.Is(err, ErrInvalidQuery) || got != (AeroAlert{}) {
				t.Fatalf("invalid callback = %+v, %v", got, err)
			}
		})
	}
}
