package feeds

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAIXMNoticeValidityAndContext(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, extra, wantKind, wantRaw string
		fixed, daily                   bool
	}{
		{"full closure", "<e:minimumFL>000</e:minimumFL><e:maximumFL>999</e:maximumFL><e:traffic>IV</e:traffic><e:selectionCode>QFALC</e:selectionCode>", "airport_closure", "", true, false},
		{"daily closure", "<e:schedule>Daily 18:00-19:00</e:schedule>", "airport_closure", "D) DLY 1800-1900", true, true},
		{"schedule exception", "<e:schedule>Daily 1800-1900 exc Nov 7</e:schedule>", AdvisoryAirportNotice, "exc Nov 7", true, false},
		{"estimated", "<e:estimatedEnd>YES</e:estimatedEnd>", AdvisoryAirportNotice, "C) 2611081930 EST", false, false},
		{"vertical limit", "<e:upperLimit>FL095</e:upperLimit>", AdvisoryAirportNotice, "G) FL095", true, false},
		{"Q-line limit", "<e:minimumFL>010</e:minimumFL>", AdvisoryAirportNotice, "", true, false},
		{"VFR only", "<e:traffic>V</e:traffic>", AdvisoryAirportNotice, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fixtureAIXMNOTAM("0181", "N", "", tc.extra)
			bodies, err := AIXMAdvisoryBodies(strings.NewReader(raw), Eurocontrol, "egtt", now.Add(-time.Minute), now)
			if err != nil || len(bodies) != 1 {
				t.Fatalf("convert: got %d envelopes, error %v", len(bodies), err)
			}
			row, err := DecodeAdvisoryIngress(bytes.NewReader(bodies[0]))
			decoded, _ := DecodeNOTAM(row.RawReason)
			if err != nil || row.ID != "egtt/C0181/26" || row.Airport != "EGLL" || row.Kind != tc.wantKind ||
				row.ObservedAt != now.Add(-time.Minute) || (row.EndsAt != nil) != tc.fixed || decoded.HasDailyClosure() != tc.daily ||
				!strings.Contains(row.RawReason, tc.wantRaw) {
				t.Fatalf("decoded row = %+v, summary = %+v, err = %v", row, decoded, err)
			}
		})
	}
}

func TestAIXMMidnightValidity(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 30, 0, 0, time.UTC)
	input := fixtureAIXMNOTAM("181", "N", "", "<e:schedule>Daily 18:00-23:59</e:schedule>")
	input = strings.ReplaceAll(input, "2611081930", "2611082359")
	bodies, err := AIXMAdvisoryBodies(strings.NewReader(input), Eurocontrol, "egtt", now.Add(-time.Minute), now)
	if err != nil || len(bodies) != 1 {
		t.Fatalf("convert: got %d envelopes, error %v", len(bodies), err)
	}
	row, err := DecodeAdvisoryIngress(bytes.NewReader(bodies[0]))
	summary, _ := DecodeNOTAM(row.RawReason)
	_, end, ok := summary.NextDailyClosure(now)
	if err != nil || row.EndsAt == nil || row.EndsAt.Format(time.RFC3339) != "2026-11-09T00:00:00Z" ||
		!ok || end.Format(time.RFC3339) != "2026-11-07T00:00:00Z" {
		t.Fatalf("end-of-day validity: row = %+v, occurrence end = %s, err = %v", row, end, err)
	}
}

func TestAIXMReplaysDistinctPublicationRevisionsNewestFirst(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 30, 0, 0, time.UTC)
	issued := func(stamp string) string { return "<e:issued>" + stamp + "</e:issued>" }
	ref := func(number string) string {
		return "<e:referredSeries>C</e:referredSeries><e:referredNumber>" + number + "</e:referredNumber><e:referredYear>2026</e:referredYear>"
	}
	older := fixtureAIXMNOTAM("181", "N", issued("2026-11-06T18:01:00Z"), "")
	replacement := fixtureAIXMNOTAM("182", "R", issued("2026-11-06T18:02:00Z"), ref("181"))
	cancel := fixtureAIXMNOTAM("183", "C", issued("2026-11-06T18:03:00Z"), ref("182"))
	stale := fixtureAIXMNOTAM("179", "N", issued("2026-11-06T11:00:00Z"), "")
	document := fixtureAIXMBatch(older, replacement, stale, replacement, cancel)
	bodies, err := AIXMAdvisoryBodies(strings.NewReader(document), FAASWIM, "egtt", time.Time{}, now)
	if err != nil || len(bodies) != 3 {
		t.Fatalf("convert: got %d envelopes, error %v", len(bodies), err)
	}
	for i, want := range []struct{ id, supersedes, kind string }{
		{"egtt/C0183/26", "egtt/C0182/26", "notam_cancellation"},
		{"egtt/C0182/26", "egtt/C0181/26", "airport_closure"},
		{"egtt/C0181/26", "", "airport_closure"},
	} {
		row, err := DecodeAdvisoryIngress(bytes.NewReader(bodies[i]))
		if err != nil || row.ID != want.id || row.SupersedesID != want.supersedes || row.Kind != want.kind {
			t.Fatalf("row %d = %+v, err %v", i, row, err)
		}
	}
	replay, err := AIXMAdvisoryBodies(strings.NewReader(document), FAASWIM, "egtt", time.Time{}, now.Add(time.Minute))
	if err != nil || len(replay) != len(bodies) {
		t.Fatalf("replay: got %d envelopes, error %v", len(replay), err)
	}
	for i := range bodies {
		if !bytes.Equal(bodies[i], replay[i]) {
			t.Fatal("relay time changed the published revision")
		}
	}

	conflict := strings.ReplaceAll(replacement, "AD closed.", "AD unavailable.")
	if _, err := AIXMAdvisoryBodies(strings.NewReader(fixtureAIXMBatch(replacement, conflict)), FAASWIM, "egtt", time.Time{}, now); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("conflicting duplicate was not rejected: %v", err)
	}
	future := strings.ReplaceAll(cancel, "2611061800", "2611061835")
	if _, err := AIXMAdvisoryBodies(strings.NewReader(future), FAASWIM, "egtt", time.Time{}, now); !errors.Is(err, ErrAdvisoryNotYetEffective) {
		t.Fatalf("future cancellation was not deferred: %v", err)
	}
	current, err := AIXMAdvisoryBodies(strings.NewReader(fixtureAIXMBatch(future, older)), FAASWIM, "egtt", time.Time{}, now)
	if !errors.Is(err, ErrAdvisoryNotYetEffective) || len(current) != 1 || !bytes.Equal(current[0], bodies[2]) {
		t.Fatalf("deferral withheld a current notice: %d envelopes, error %v", len(current), err)
	}
	if current, err := AIXMAdvisoryBodies(strings.NewReader(fixtureAIXMBatch(future, older, conflict, replacement)), FAASWIM, "egtt", time.Time{}, now); !errors.Is(err, ErrInvalidQuery) || len(current) != 0 {
		t.Fatalf("deferral hid conflicting input: %d envelopes, error %v", len(current), err)
	}
	if _, err := AIXMAdvisoryBodies(strings.NewReader(future), FAASWIM, "egtt", time.Time{}, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("effective cancellation still deferred: %v", err)
	}
}

func fixtureAIXMNOTAM(number, action, issued, extra string) string {
	end := "<e:effectiveEnd>2611081930</e:effectiveEnd><e:text>AD closed.</e:text>"
	if action == "C" {
		end = ""
	}
	return fmt.Sprintf(`<e:NOTAM xmlns:e="%s/event"><e:series>C</e:series><e:number>%s</e:number><e:year>2026</e:year>
<e:type>%s</e:type>%s<e:scope>A</e:scope><e:location>EGLL</e:location><e:effectiveStart>2611061800</e:effectiveStart>%s%s</e:NOTAM>`,
		aixm511, number, action, issued, end, extra)
}

func fixtureAIXMBatch(notices ...string) string {
	return `<m:AIXMBasicMessage xmlns:m="` + aixm511 + `/message" xmlns:e="` + aixm511 + `/event">
<m:hasMember><e:Event><e:timeSlice><e:EventTimeSlice>` +
		"<e:notification>" + strings.Join(notices, "</e:notification><e:notification>") + "</e:notification>" +
		"</e:EventTimeSlice></e:timeSlice></e:Event></m:hasMember></m:AIXMBasicMessage>"
}
