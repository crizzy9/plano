package feeds

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDecodeNOTAMConservesClosureScopeAndQualifiers(t *testing.T) {
	for _, test := range []struct {
		name, raw, scope, phrase string
		fullAirportClosure       bool
	}{
		{"full airport", "!SFO SFO AD AP CLSD 2611051900-2611052300", "airport", "Airport closed 2611051900-2611052300", true},
		{"conditional airport", "!LAX 05/277 LAX AD AP CLSD TO NON SKED TRANSIENT GA ACFT EXC 24HR PPR", "airport", "Airport closed to non-scheduled visiting general aviation aircraft except 24 hr prior permission required", false},
		{"recurring airport", "AD AP CLSD DLY 0730-1415 2611030730-2611081415", "airport", "Airport closed daily 0730-1415", false},
		{"terminal only", "AD AP NOT ATTENDED MAIN TERMINAL CLSD", "airport", "MAIN TERMINAL closed", false},
		{"partial runway", "!JFK JFK RWY 02 FIRST 1000FT CLSD EXC TAX", "runway", "Runway 02 FIRST 1000 ft closed except taxiing", false},
		{"taxiway segment", "TWY A BTN TWY B AND RWY 01/19 CLSD", "taxiway", "Taxiway A between taxiway B and runway 01/19 closed", false},
		{"lighting only", "TWY C EDGE LGT OBSC", "taxiway", "Taxiway C EDGE light obscured", false},
		{"apron", "APRON NORTH APN E 100FT CLSD", "apron", "Apron NORTH apron E 100 ft closed", false},
		{"communications outage", "!INW INW COM REMOTE COM OUTLET 122.6 U/S", "communications", "122.6 out of service", false},
		{"service availability", "SVC TWR NOT AVBL", "service", "Service tower not available", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := DecodeNOTAM(test.raw)
			if !ok || got.Scope != test.scope || got.FullAirportClosure != test.fullAirportClosure || !strings.Contains(got.Text, test.phrase) {
				t.Fatalf("decoded = %+v, %t", got, ok)
			}
		})
	}
}

func TestDecodeNOTAMUsesICAOTextRatherThanQCode(t *testing.T) {
	raw := `(A1234/26 NOTAMN
Q) EGTT/QFALC/IV/NBO/A/000/999/5129N00028W005
A) EGLL B) 2611051900 C) 2611052300
D) DLY 1900-2000
E) RWY 09L/27R CLSD (EXC TAX))`
	got, ok := DecodeNOTAM(raw)
	if !ok || got.Scope != "runway" || got.FullAirportClosure || got.Text != "Runway 09L/27R closed (except taxiing). Published schedule daily 1900-2000." {
		t.Fatalf("ICAO notice = %+v, %t", got, ok)
	}
	for _, raw := range []string{
		"Q) EGTT/QFALC/IV/NBO/A/000/999/5129N00028W005 A) EGLL",
		"!FDC 2/9700 DIK ODP EXAMPLE REFERENCES RWY 25 CLSD",
		"An unrelated sentence mentioning RWY 09L/27R CLSD",
		strings.Repeat("A", 8193),
	} {
		if got, ok := DecodeNOTAM(raw); ok {
			t.Fatalf("unsupported notice decoded as %+v", got)
		}
	}
	got, ok = DecodeNOTAM("(A1234/26 NOTAMC A1233/26\nA) EGLL\nE) AD CLSD)")
	if !ok || got.Scope != "cancellation" || got.FullAirportClosure || strings.Contains(got.Text, "closed") {
		t.Fatalf("cancelled notice decoded as %+v, %t", got, ok)
	}
}

func TestNOTAMSummaryAppliesToPushAndPollBridge(t *testing.T) {
	const raw = "!SFO SFO AD AP CLSD EXC 1HR PPR\nCTC EXAMPLE"
	for _, test := range []struct {
		name, kind, reason, wantKind, wantReason string
	}{
		{"correct broad closure", "airport_closure", "The airport is closed", "airport_restriction", "Airport closed except 1 hr prior permission required contact EXAMPLE"},
		{"fill missing explanation", "airport_restriction", "", "airport_restriction", "Airport closed except 1 hr prior permission required contact EXAMPLE"},
		{"preserve operator explanation", "airport_restriction", "Call the airport before departure", "airport_restriction", "Call the airport before departure"},
		{"leave traffic advisory alone", "ground_delay", "wind", "ground_delay", "wind"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields := Advisory{ID: "test-notam", Airport: "KSFO", Kind: test.kind, Reason: test.reason, RawReason: raw, ObservedAt: instant(t, "2026-11-05T19:00:00Z")}
			row, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			incoming := `{"version":1,"source":"faa_swim","advisory":` + string(row) + `}`
			push, err := DecodeAdvisoryIngress(strings.NewReader(incoming))
			if err != nil {
				t.Fatal(err)
			}
			snapshot := `{"version":1,"advisories":[` + string(row) + `]}`
			gateway, err := NewAdvisoryGateway("https://fixture.invalid/advisories", "", FAASWIM, &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, snapshot)
			})})
			if err != nil {
				t.Fatal(err)
			}
			poll, err := gateway.Advisories(context.Background())
			if err != nil || len(poll) != 1 {
				t.Fatalf("poll = %+v, %v", poll, err)
			}
			for _, got := range []Advisory{push, poll[0]} {
				if got.Kind != test.wantKind || got.Reason != test.wantReason || got.RawReason != raw || got.Source != FAASWIM || got.Airport != "KSFO" || got.DelayMinutes != nil || got.StartsAt != nil || got.EndsAt != nil {
					t.Fatalf("normalized advisory = %+v", got)
				}
			}
		})
	}
}
