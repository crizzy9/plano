package feeds

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAdvisoryTFMSObservationWindow(t *testing.T) {
	now := time.Date(2026, time.November, 6, 22, 0, 0, 0, time.UTC)
	program := Advisory{
		ID: "tfms/gdp/ORD", Source: FAASWIM, Airport: "ORD", Kind: "ground_delay", Direction: "arrival", AirportContextOnly: true,
		ObservedAt: now.Add(-8 * time.Hour), StartsAt: new(now.Add(-7 * time.Hour)), EndsAt: new(now.Add(time.Hour)),
	}
	if !program.IsTFMSAirportProgram() || !program.ObservationAllowed(now) || !program.Expiry().Equal(*program.EndsAt) {
		t.Fatalf("eight-hour TFMS observation did not retain its published end: %+v", program)
	}
	ended := program
	ended.EndsAt = new(now.Add(-time.Minute))
	if !ended.ObservationAllowed(now) || !ended.Expiry().Equal(*ended.EndsAt) {
		t.Fatalf("ended TFMS revision did not retain its ordering timestamp and expiry: %+v", ended)
	}
	stop := program
	stop.ID, stop.Kind = "tfms/gs/ORD", "ground_stop"
	if !stop.IsTFMSAirportProgram() {
		t.Fatalf("canonical ground stop did not qualify: %+v", stop)
	}
	for _, tc := range []struct {
		name   string
		change func(*Advisory)
	}{
		{"noncanonical key", func(a *Advisory) { a.ID = "tfms/gdp/KORD" }},
		{"other source", func(a *Advisory) { a.Source = Eurocontrol }},
		{"invalid airport", func(a *Advisory) { a.Airport, a.ID = "ord", "tfms/gdp/ord" }},
		{"flight context", func(a *Advisory) { a.AirportContextOnly = false }},
		{"departure direction", func(a *Advisory) { a.Direction = "departure" }},
		{"missing start", func(a *Advisory) { a.StartsAt = nil }},
		{"zero start", func(a *Advisory) { a.StartsAt = new(time.Time{}) }},
		{"missing end", func(a *Advisory) { a.EndsAt = nil }},
		{"unordered period", func(a *Advisory) { a.EndsAt = a.StartsAt }},
		{"linked predecessor", func(a *Advisory) { a.SupersedesID = "other" }},
		{"assigned delay", func(a *Advisory) { a.DelayMinutes = new(0) }},
		{"withdrawal", func(a *Advisory) { a.Kind = "cancelled" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ordinary := program
			tc.change(&ordinary)
			if ordinary.IsTFMSAirportProgram() || ordinary.ObservationAllowed(now) {
				t.Fatalf("ineligible revision received extended lifetime: %+v", ordinary)
			}
		})
	}

	program.EndsAt = new(now.Add(72 * time.Hour))
	if want := program.ObservedAt.Add(TFMSAdvisoryMaxAge); !program.Expiry().Equal(want) {
		t.Fatalf("extended expiry = %v, want %v", program.Expiry(), want)
	}
	for _, tc := range []struct {
		name string
		row  Advisory
		age  time.Duration
	}{
		{"TFMS", program, TFMSAdvisoryMaxAge},
		{"ordinary", Advisory{Source: FAASWIM, ID: "GS-1"}, AdvisoryMaxAge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, bound := range []struct {
				offset time.Duration
				allow  bool
			}{
				{-tc.age, true},
				{-tc.age - time.Nanosecond, false},
				{30 * time.Second, true},
				{30*time.Second + time.Nanosecond, false},
			} {
				row := tc.row
				row.ObservedAt = now.Add(bound.offset)
				if got := row.ObservationAllowed(now); got != bound.allow {
					t.Errorf("observation at offset %v allowed = %t, want %t", bound.offset, got, bound.allow)
				}
			}
		})
	}
}

func TestBridgeAdvisorySupersedesID(t *testing.T) {
	const id = `"id":"EGTT:A1234/26"`
	const airport = `"airport":"EGLL"`
	const base = id + `,` + airport + `,"kind":"ground_delay","observedAt":"2026-11-05T18:00:00Z"`
	const link = `,"supersedesId":"  EGTT:A1233/26  "`
	const cancellation = `,"rawReason":"(A1234/26 NOTAMC A1233/26\nA) EGLL\nB) 2611051800\nE) CANCELLED)"`
	const replacement = `,"rawReason":"(A1234/26 NOTAMR A1233/26\nA) EGLL\nB) 2611051800\nC) 2611052000\nE) OBST TOWER)"`
	for _, tc := range []struct {
		name, fields, wantLink, wantKind string
	}{
		{"operator link without raw", base + link, "EGTT:A1233/26", "ground_delay"},
		{"replacement", base + link + replacement, "EGTT:A1233/26", "ground_delay"},
		{"compact replacement", base + link + strings.Replace(replacement, "/26 NOTAMR", "/26NOTAMR", 1), "EGTT:A1233/26", "ground_delay"},
		{"cancellation", base + link + cancellation, "EGTT:A1233/26", "notam_cancellation"},
		{"header-only cancellation", base + link + `,"rawReason":"(A1234/26NOTAMC A1233/26)"`, "EGTT:A1233/26", "notam_cancellation"},
		{"unlinked cancellation", base + cancellation, "", "ground_delay"},
		{"unsafe link", base + `,"supersedesId":"EGTT:A12\n33/26"`, "", ""},
		{"blank link", base + `,"supersedesId":"  "`, "", ""},
		{"self link", base + `,"supersedesId":" EGTT:A1234/26 "`, "", ""},
		{"unsafe linked ID", strings.Replace(base, id, `"id":"unsafe ID"`, 1) + link, "", ""},
		{"area-only link", strings.Replace(base, airport, `"area":"EGTT"`, 1) + link, "", ""},
		{"cancellation airport mismatch", strings.Replace(base, airport, `"airport":"EGKK"`, 1) + link + cancellation, "", ""},
		{"replacement airport mismatch", strings.Replace(base, airport, `"airport":"EGKK"`, 1) + link + replacement, "", ""},
		{"compact replacement airport mismatch", strings.Replace(base, airport, `"airport":"EGKK"`, 1) + link + strings.Replace(replacement, "/26 NOTAMR", "/26NOTAMR", 1), "", ""},
		{"invalid Item A", base + link + strings.Replace(cancellation, `A) EGLL`, `A) E123`, 1), "", ""},
		{"empty Item A", base + link + strings.Replace(cancellation, `A) EGLL`, `A)`, 1), "", ""},
		{"duplicate Item A", base + link + strings.Replace(cancellation, `A) EGLL`, `A) EGLL\nA) EGLL`, 1), "", ""},
		{"multiple Item A locations", base + link + strings.Replace(cancellation, `A) EGLL`, `A) EGLL EGKK`, 1), "", ""},
		{"out-of-order Item A", base + link + strings.Replace(cancellation, `A) EGLL\nB) 2611051800`, `B) 2611051800\nA) EGLL`, 1), "", ""},
		{"missing reference", base + link + strings.Replace(cancellation, `NOTAMC A1233/26`, `NOTAMC`, 1), "", ""},
		{"malformed reference", base + link + strings.Replace(cancellation, `NOTAMC A1233/26`, `NOTAMC ABC`, 1), "", ""},
		{"extra reference", base + link + strings.Replace(cancellation, `NOTAMC A1233/26`, `NOTAMC A1233/26 A1232/26`, 1), "", ""},
		{"different-series reference", base + link + strings.Replace(cancellation, `NOTAMC A1233/26`, `NOTAMC B1233/26`, 1), "", ""},
		{"same-serial reference", base + link + strings.Replace(cancellation, `NOTAMC A1233/26`, `NOTAMC A1234/26`, 1), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := `{` + tc.fields + `}`
			pushed, pushErr := DecodeAdvisoryIngress(strings.NewReader(`{"version":1,"source":"eurocontrol_nm","advisory":` + row + `}`))
			client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, `{"version":1,"advisories":[`+row+`]}`)
			})}
			gateway, err := NewAdvisoryGateway("https://fixture.invalid/advisories", "", Eurocontrol, client)
			if err != nil {
				t.Fatal(err)
			}
			polled, pollErr := gateway.Advisories(t.Context())
			if tc.wantKind == "" {
				if !errors.Is(pushErr, ErrInvalidQuery) || !errors.Is(pollErr, ErrUnavailable) {
					t.Fatalf("expected push and poll to reject the link, got %v and %v", pushErr, pollErr)
				}
				return
			}
			if pushErr != nil || pollErr != nil || len(polled) != 1 {
				t.Fatalf("push error = %v, poll rows = %d, poll error = %v", pushErr, len(polled), pollErr)
			}
			for _, got := range []Advisory{pushed, polled[0]} {
				if got.Source != Eurocontrol || got.Airport != "EGLL" || got.SupersedesID != tc.wantLink || got.Kind != tc.wantKind {
					t.Errorf("advisory = %+v, want link %q and kind %q", got, tc.wantLink, tc.wantKind)
				}
			}
		})
	}
}

func TestDecodeAdvisoryIngressNormalizesDisplayAndPreservesRaw(t *testing.T) {
	input := `{
		"version":1,"source":"eurocontrol_nm",
		"advisory":{
			"id":"  NM:A1234/26  ","airport":" egll ","area":" EGTT/LON ",
			"kind":"ground_delay","direction":"arrival","delayMinutes":30,
			"reason":"  Capacity\u0000 reduced\t due to \u202Evisibility < 800 m\n ",
			"rawReason":"RAW\n VIS < 800\u202e",
			"observedAt":"2026-11-05T19:04:00+01:00","startsAt":"2026-11-05T18:00:00Z"
		}
	}`
	got, err := DecodeAdvisoryIngress(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "NM:A1234/26" || got.Source != Eurocontrol || got.Airport != "EGLL" || got.Area != "EGTT/LON" ||
		got.Kind != "ground_delay" || got.Direction != "arrival" || got.DelayMinutes == nil || *got.DelayMinutes != 30 {
		t.Fatalf("advisory identity and impact = %+v", got)
	}
	if got.Reason != "Capacity reduced due to visibility ＜ 800 m" || got.RawReason != "RAW\n VIS < 800\u202e" {
		t.Fatalf("reason = %q, raw reason = %q", got.Reason, got.RawReason)
	}
	if got.ObservedAt.Location() != time.UTC || got.ObservedAt.Format(time.RFC3339) != "2026-11-05T18:04:00Z" ||
		got.StartsAt == nil || got.EndsAt != nil {
		t.Fatalf("advisory time and open interval = %+v", got)
	}
}

func TestDecodeAdvisoryIngressAllowsAreaOnlyAndUnknownInterval(t *testing.T) {
	input := `{"version":1,"source":"faa_swim","advisory":{"id":"FCA0001","area":"NORTHEAST 1","kind":"airspace_flow","observedAt":"2026-11-05T18:00:00Z"}}`
	got, err := DecodeAdvisoryIngress(strings.NewReader(input))
	if err != nil || got.Source != FAASWIM || got.Area != "NORTHEAST 1" || got.Airport != "" || got.StartsAt != nil || got.EndsAt != nil || got.DelayMinutes != nil {
		t.Fatalf("area advisory = %+v, %v", got, err)
	}
}

func TestDecodeAdvisoryIngressRejectsInvalidRecords(t *testing.T) {
	validFields := `"id":"FCA0001","airport":"JFK","kind":"ground_stop","observedAt":"2026-11-05T18:00:00Z"`
	envelope := func(fields string) string { return `{"version":1,"source":"faa_swim","advisory":{` + fields + `}}` }
	for name, input := range map[string]string{
		"unknown source": strings.Replace(envelope(validFields), `"faa_swim"`, `"faa_nas"`, 1),
		"inner source":   envelope(validFields + `,"source":"eurocontrol_nm"`),
		"version":        strings.Replace(envelope(validFields), `"version":1`, `"version":2`, 1),
		"missing scope":  envelope(strings.Replace(validFields, `"airport":"JFK",`, "", 1)),
		"unsafe airport": strings.Replace(envelope(validFields), `"JFK"`, `"JK/F"`, 1),
		"unsafe ID":      strings.Replace(envelope(validFields), `"FCA0001"`, `"FCA\u00000001"`, 1),
		"bad area":       envelope(validFields + `,"area":"NORTH\nEAST"`),
		"bad direction":  envelope(validFields + `,"direction":"taxi"`),
		"bad duration":   envelope(validFields + `,"delayMinutes":1441`),
		"bad chronology": envelope(validFields + `,"startsAt":"2026-11-05T19:00:00Z","endsAt":"2026-11-05T18:00:00Z"`),
		"zero end":       envelope(validFields + `,"endsAt":"0001-01-01T00:00:00Z"`),
		"second object":  envelope(validFields) + `{}`,
		"invalid UTF-8":  envelope(validFields + `,"reason":"` + string([]byte{0xff}) + `"`),
		"too large":      envelope(validFields) + strings.Repeat(" ", MaxAdvisoryIngressBytes),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeAdvisoryIngress(strings.NewReader(input))
			if !errors.Is(err, ErrInvalidQuery) || got != (Advisory{}) {
				t.Fatalf("invalid advisory = %+v, %v", got, err)
			}
		})
	}
}
