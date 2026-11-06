package feeds

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDecodeNOTAMConservesClosureScopeAndQualifiers(t *testing.T) {
	for _, test := range []struct {
		name, raw, scope, phrase string
		fullAirportClosure       bool
	}{
		{"full airport", "!SFO SFO AD AP CLSD 2611051900-2611052300", "airport", "Airport closed 2611051900-2611052300", true},
		{"conditional airport", "!LAX 05/277 LAX AD AP CLSD TO NON SKED TRANSIENT GA ACFT EXC 24HR PPR", "airport", "Airport closed to non-scheduled visiting general aviation aircraft except 24 hr prior permission required", false},
		{"recurring airport", "AD AP CLSD DLY 0730-1415 2611030730-2611081415", "airport", "Airport closed daily 0730-1415", true},
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
	if !ok || got.Scope != "runway" || got.Airport != "EGLL" || got.FullAirportClosure || got.Text != "Runway 09L/27R closed (except taxiing). Published schedule daily 1900-2000." {
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

func TestDecodeNOTAMValidity(t *testing.T) {
	const start = "2026-11-05T19:00:00Z"
	const end = "2026-11-05T23:00:00Z"
	for _, test := range []struct {
		name, raw, start, end string
		present, fixed, full  bool
	}{
		{"standalone", "AD CLSD", "", "", false, false, true},
		{"domestic", "!SFO SFO AD AP CLSD 2611051900-2611052300", start, end, true, true, true},
		{"domestic year rollover", "!SFO SFO AD AP CLSD 6812312359 ‐ 6901010000", "2068-12-31T23:59:00Z", "2069-01-01T00:00:00Z", true, true, true},
		{"domestic estimate", "!SFO SFO AD AP CLSD 2611051900-2611052300EST", start, end, true, false, true},
		{"domestic permanent", "!SFO SFO AD AP CLSD 2611051900 - PERM", start, "", true, false, true},
		{"domestic indefinite", "!SFO SFO AD AP CLSD 2611051900-UFN", start, "", true, false, true},
		{"domestic truncated end", "!SFO SFO AD AP CLSD 2611051900-261105230", start, "", true, false, false},
		{"ICAO", "(A1234/26 NOTAMN\nA) EGLL B) 2611051900 C) 2611052300\nE) AD CLSD)", start, end, true, true, true},
		{"ICAO recurring", "A) EGLL B) 2611051900 C) 2611052300 D) DLY 1900-2000 E) AD CLSD", start, end, true, true, true},
		{"ICAO empty schedule", "A) EGLL B) 2611051900 C) 2611052300 D) E) AD CLSD", start, end, true, true, false},
		{"ICAO estimate", "A) EGLL B) 2611051900 C) 2611052300 EST E) AD CLSD", start, end, true, false, true},
		{"ICAO partial", "A) EGLL C) 2611052300 E) AD CLSD", "", end, true, false, true},
		{"ICAO missing bounds", "A) EGLL E) AD CLSD", "", "", true, false, true},
		{"ICAO header missing bounds", "A1234/26 NOTAMN\nE) AD CLSD", "", "", true, false, true},
		{"ICAO missing location", "A1234/26 NOTAMN\nB) 2611051900 C) 2611052300 E) AD CLSD", "", "", true, false, true},
		{"ICAO ambiguous location", "A) EGLL EGGW B) 2611051900 C) 2611052300 E) AD CLSD", "", "", true, false, true},
		{"ICAO embedded domestic header", "A) EGLL B) 2611051900 C) 2611052300 E) !SFO SFO AD CLSD", "", "", true, false, true},
		{"ICAO embedded domestic range", "A) EGLL E) AD CLSD 2611051900-2611052300", "", "", true, false, true},
		{"ICAO contradictory embedded range", "A) EGLL B) 2611051900 C) 2611052300 E) AD CLSD 2611051930-2611052300EST", "", "", true, false, true},
		{"ICAO empty start", "A) EGLL B) C) 2611052300 E) AD CLSD", "", end, true, false, true},
		{"ICAO duplicate", "A) EGLL B) 2611051900 C) 2611052300 C) 2611052300 E) AD CLSD", "", "", true, false, true},
		{"ICAO out of order", "A) EGLL C) 2611052300 B) 2611051900 E) AD CLSD", "", "", true, false, true},
		{"ICAO timing in free text", "A) EGLL E) AD CLSD B) 2611051900 C) 2611052300", "", "", true, false, true},
		{"ICAO calendar overflow", "A) EGLL B) 2602301900 C) 2611052300 E) AD CLSD", "", end, true, false, true},
		{"ICAO zero day", "A) EGLL B) 2611001900 C) 2611052300 E) AD CLSD", "", end, true, false, true},
		{"ICAO hour overflow", "A) EGLL B) 2611051900 C) 2611052400 E) AD CLSD", start, "", true, false, true},
		{"ICAO reverse bounds", "A) EGLL B) 2611052300 C) 2611051900 E) AD CLSD", end, start, true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := DecodeNOTAM(test.raw)
			if !ok || got.HasValidity != test.present || got.FixedValidity != test.fixed || got.FullAirportClosure != test.full {
				t.Fatalf("decoded = %+v, %t", got, ok)
			}
			checkTime := func(name string, got *time.Time, want string) {
				t.Helper()
				if got == nil && want == "" {
					return
				}
				if got == nil || got.Location() != time.UTC || got.Format(time.RFC3339) != want {
					t.Errorf("%s = %v, want UTC %q", name, got, want)
				}
			}
			checkTime("start", got.StartsAt, test.start)
			checkTime("end", got.EndsAt, test.end)
		})
	}
}

func TestNextDailyNOTAMClosure(t *testing.T) {
	const domestic = "!SNA 11/003 SNA AD AP CLSD DLY 0730-1415 2611030730-2611081415"
	const overnight = "(A1234/26 NOTAMN\nA) EGLL B) 2611052330 C) 2611070030 D) DLY 2200-0200\nE) AD CLSD)"
	for _, tc := range []struct{ name, raw, at, start, end string }{
		{"before notice validity", domestic, "2026-11-01T12:00:00Z", "2026-11-03T07:30:00Z", "2026-11-03T14:15:00Z"},
		{"within daily window", domestic, "2026-11-06T08:00:00Z", "2026-11-06T07:30:00Z", "2026-11-06T14:15:00Z"},
		{"exact daily end", domestic, "2026-11-06T14:15:00Z", "2026-11-07T07:30:00Z", "2026-11-07T14:15:00Z"},
		{"before daily start", domestic, "2026-11-07T05:15:00Z", "2026-11-07T07:30:00Z", "2026-11-07T14:15:00Z"},
		{"exact validity end", domestic, "2026-11-08T14:15:00Z", "", ""},
		{"clipped overnight start in UTC", overnight, "2026-11-05T19:00:00-05:00", "2026-11-05T23:30:00Z", "2026-11-06T02:00:00Z"},
		{"clipped overnight end", overnight, "2026-11-06T23:00:00Z", "2026-11-06T22:00:00Z", "2026-11-07T00:30:00Z"},
		{"after clipped end", overnight, "2026-11-07T01:00:00Z", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, decoded := DecodeNOTAM(tc.raw)
			if !decoded || !summary.HasDailyClosure() || !summary.FullAirportClosure || !summary.MatchesValidity(nil, nil) {
				t.Fatalf("daily closure = %+v, %t", summary, decoded)
			}
			start, end, ok := summary.NextDailyClosure(instant(t, tc.at))
			if tc.start == "" {
				if ok || !start.IsZero() || !end.IsZero() {
					t.Fatalf("unexpected occurrence %v–%v", start, end)
				}
				return
			}
			if !ok || start.Format(time.RFC3339) != tc.start || end.Format(time.RFC3339) != tc.end {
				t.Fatalf("occurrence = %v–%v, %t, want %s–%s", start, end, ok, tc.start, tc.end)
			}
		})
	}
}

func TestUnqualifiedDailyNOTAMClosureOnly(t *testing.T) {
	const icao = "A) EGLL B) 2611051900 C) 2611072000 "
	for _, raw := range []string{
		"AD AP CLSD DLY 1900-2000",
		"AD AP CLSD DLY 1900-2000 2611051900-2611072000EST",
		"AD AP CLSD DLY 1860-2000 2611051900-2611072000",
		"AD AP CLSD DLY 1900-2400 2611051900-2611072000",
		"AD AP CLSD DLY 1900-1900 2611051900-2611072000",
		"AD AP CLSD DLY 1900-2000 2611052100-2611061800",
		"AD AP CLSD EXC PPR DLY 1900-2000 2611051900-2611072000",
		"RWY 01 CLSD DLY 1900-2000 2611051900-2611072000",
		icao + "D) MON-FRI 1900-2000 E) AD CLSD",
		icao + "D) DLY 1900-2000 2100-2200 E) AD CLSD",
		icao + "D) DLY 1900-2000 D) DLY 2000-2100 E) AD CLSD",
		icao + "D) DLY 1900-2000 E) AD CLSD EXC PPR",
		icao + "D) DLY 1900-2000 E) AD CLSD 2611051900-2611072000",
		icao + "D) DLY 1900-2000 E) AD CLSD F) 1500FT G) 3000FT",
	} {
		if summary, ok := DecodeNOTAM(raw); !ok || summary.HasDailyClosure() || summary.FullAirportClosure {
			t.Errorf("ambiguous notice %q = %+v, %t", raw, summary, ok)
		}
	}
}

func TestDailyNOTAMClosureBridgePreservesPublishedBounds(t *testing.T) {
	const raw = "A) EGLL B) 2611051900 C) 2611072000 D) DLY 1900-2000 E) AD CLSD"
	const reason = "Aerodrome closed. Published schedule daily 1900-2000."
	start, end := instant(t, "2026-11-05T19:00:00Z"), instant(t, "2026-11-07T20:00:00Z")
	for _, tc := range []struct {
		name       string
		supplied   *time.Time
		wantEnd    time.Time
		compatible bool
	}{
		{"raw dates only", nil, end, true},
		{"matching end", &end, end, true},
		{"conflicting end", new(end.Add(time.Hour)), end.Add(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := Advisory{ID: "daily-closure", Airport: "EGLL", Kind: "airport_closure", RawReason: raw,
				ObservedAt: start.Add(-time.Hour), EndsAt: tc.supplied}
			wire, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			push, err := DecodeAdvisoryIngress(strings.NewReader(`{"version":1,"source":"eurocontrol_nm","advisory":` + string(wire) + `}`))
			if err != nil {
				t.Fatal(err)
			}
			gateway, err := NewAdvisoryGateway("https://fixture.invalid/advisories", "", Eurocontrol, &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, `{"version":1,"advisories":[`+string(wire)+`]}`)
			})})
			if err != nil {
				t.Fatal(err)
			}
			polled, err := gateway.Advisories(t.Context())
			if err != nil || len(polled) != 1 {
				t.Fatalf("poll = %+v, %v", polled, err)
			}
			for _, got := range []Advisory{push, polled[0]} {
				summary, ok := DecodeNOTAM(got.RawReason)
				if !ok || !summary.HasDailyClosure() || summary.MatchesValidity(got.StartsAt, got.EndsAt) != tc.compatible ||
					got.Kind != "airport_closure" || got.Reason != reason || got.EndsAt == nil || !got.EndsAt.Equal(tc.wantEnd) {
					t.Fatalf("daily closure = %+v", got)
				}
				if tc.compatible && (got.StartsAt == nil || !got.StartsAt.Equal(start)) {
					t.Fatalf("published start was lost: %+v", got)
				}
			}
		})
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
