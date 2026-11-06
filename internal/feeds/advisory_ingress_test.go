package feeds

import (
	"errors"
	"strings"
	"testing"
	"time"
)

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
