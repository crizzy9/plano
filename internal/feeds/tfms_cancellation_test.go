package feeds

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func tfmsCancellationFixture(number, origin, sent, title, body, attrs string) string {
	payload := fmt.Sprintf(`<flow:generalAdvisory>
  <c:advisoryNumber>%s</c:advisoryNumber><c:origin>%s</c:origin><c:dateSent>%s</c:dateSent>
  <c:effectivePeriod><t:startTime>2026-11-06T00:00:00Z</t:startTime><t:endTime>2026-11-06T00:01:00Z</t:endTime></c:effectivePeriod>
  <c:advisoryTitle>%s</c:advisoryTitle><c:advisoryText>%s</c:advisoryText>
</flow:generalAdvisory>`, number, origin, sent, title, body)
	return tfmsFixture("GADV", payload, attrs)
}

func TestTFMSAdvisoryBodiesScopedCancellations(t *testing.T) {
	for _, tc := range []struct {
		name, family, airport, number, sent, title, body, start, end, date string
	}{
		{"GS hyphen", "gs", "LGA", "059", "2026-09-03T11:56:17Z",
			"ATCSCC\u00a0ADVZY 059 LGA/ZNY 09/03/2026 CDM GS CNX",
			"CTL ELEMENT: LGA\nELEMENT TYPE: APT\nADL TIME: 1151Z\nGS CNX PERIOD: 03/1151Z - 03/1323Z\nCOMMENTS: PRIVATE-FIXTURE",
			"2026-09-03T11:51:00Z", "2026-09-03T13:23:00Z", "20260903"},
		{"GS colonless", "gs", "LGA", "0059", "2026-09-03T11:56:17Z",
			"ATCSCC ADVZY 059 LGA/ZNY 9/3/2026 CDM GROUND STOP CNX",
			"CTL ELEMENT LGA\nELEMENT TYPE APT\nADL TIME 1151Z\nGS CNX PERIOD 03/1151Z  03/1323Z\nCOMMENTS PRIVATE-FIXTURE",
			"2026-09-03T11:51:00Z", "2026-09-03T13:23:00Z", "20260903"},
		{"GDP UTC year rollover", "gdp", "LGA", "0059", "2027-01-01T00:20:13.4Z",
			"ATCSCC ADVZY 059 LGA/ZNY 1/1/2027 CDM GROUND DELAY PROGRAM CNX",
			"CTL ELEMENT: LGA\nELEMENT TYPE: APT\nADL TIME: 2330Z\nGDP CNX PERIOD: 31/2330Z - 01/0530Z\nDISREGARD EDCTS FOR DEST LGA\nCOMMENTS: PRIVATE-FIXTURE",
			"2026-12-31T23:30:00Z", "2027-01-01T05:30:00Z", "20270101"},
		{"GS airport code and repeated heading", "gs", "CYUL", "0010", "2026-09-03T00:55:00Z",
			"ATCSCC ADVZY 010 CYUL/CZU 09/03/2026 CDM GS CNX",
			"EVENT TIME: 03/0055 - 03/0115\nCDM GS CNX\nCTL ELEMENT: CYUL\nELEMENT TYPE: APT\nADL TIME: 0049Z\nGS CNX PERIOD: 03/0049Z - 03/0317Z",
			"2026-09-03T00:49:00Z", "2026-09-03T03:17:00Z", "20260903"},
		{"GDP abbreviated event time", "gdp", "CYYZ", "0016", "2026-08-07T01:07:00Z",
			"ATCSCC ADVZY 016 CYYZ/CZY 08/07/2026 CDM GROUND DELAY PROGRAM CNX",
			"EVENT TIME: 07/0100 - 0505\nCTL ELEMENT: CYYZ\nELEMENT TYPE: APT\nADL TIME: 0100Z\nGDP CNX PERIOD: 07/0100Z - 07/0505Z",
			"2026-08-07T01:00:00Z", "2026-08-07T05:05:00Z", "20260807"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := tfmsCancellationFixture(tc.number, "ATCSCC", tc.sent, tc.title, tc.body, `sourceFacility="TSS"`)
			bodies, err := TFMSAdvisoryBodies(strings.NewReader(doc))
			if err != nil || len(bodies) != 1 {
				t.Fatalf("converter returned %d bodies, error = %v", len(bodies), err)
			}
			a, err := DecodeAdvisoryIngress(bytes.NewReader(bodies[0]))
			key := tc.family + "/" + tc.airport
			serial := tc.number[len(tc.number)-3:]
			if err != nil || !a.IsTFMSProgramCancellation() || a.Source != FAASWIM || a.Airport != tc.airport ||
				a.TFMSProgramID != "tfms/"+key || a.ID != "tfms/cnx/"+key+"/"+tc.date+"/"+serial ||
				a.RawReason != "" || bytes.Contains(bodies[0], []byte("PRIVATE-FIXTURE")) {
				t.Fatalf("unexpected scoped cancellation = %+v, error = %v", a, err)
			}
			if a.ObservedAt.Format(time.RFC3339Nano) != tc.sent || a.StartsAt.Format(time.RFC3339) != tc.start || a.EndsAt.Format(time.RFC3339) != tc.end {
				t.Fatalf("unexpected timing = %+v", a)
			}
			refresh := tfmsCancellationFixture(tc.number, "ATCSCC", tc.sent, tc.title, tc.body, `refresh="RFRS" sourceTimeStamp="2027-01-01T06:00:00Z"`)
			again, err := TFMSAdvisoryBodies(strings.NewReader(refresh))
			if err != nil || len(again) != 1 || !bytes.Equal(again[0], bodies[0]) {
				t.Fatalf("replay changed the cancellation, error = %v", err)
			}
		})
	}
}

func TestTFMSAdvisoryBodiesRejectsAmbiguousCancellations(t *testing.T) {
	const title = "ATCSCC ADVZY 059 LGA/ZNY 09/03/2026 CDM GS CNX"
	const body = "CTL ELEMENT: LGA\nELEMENT TYPE: APT\nADL TIME: 1151Z\nGS CNX PERIOD: 03/1151Z - 03/1323Z"
	valid := tfmsCancellationFixture("0059", "ATCSCC", "2026-09-03T11:56:17Z", title, body, "")
	for _, tc := range []struct {
		name, from, to string
		want           error
	}{
		{"proposed title", "CDM GS CNX", "CDM PROPOSED GS CNX", ErrAdvisoryIgnored},
		{"ad hoc title", "CDM GS CNX", "LGA GROUND STOP CANCELLATION", ErrAdvisoryIgnored},
		{"unrelated title", "CDM GS CNX", "CDM GROUND STOP", ErrAdvisoryIgnored},
		{"wrong advisory number", ">0059<", ">0060<", ErrInvalidQuery},
		{"wrong airport", "CTL ELEMENT: LGA", "CTL ELEMENT: JFK", ErrInvalidQuery},
		{"restricted facilities", "</c:advisoryText>", "\nRELEASED FACILITIES: ZBW</c:advisoryText>", ErrInvalidQuery},
		{"wrong element type", "ELEMENT TYPE: APT", "ELEMENT TYPE: FCA", ErrInvalidQuery},
		{"wrong issue date", "09/03/2026 CDM", "09/04/2026 CDM", ErrInvalidQuery},
		{"wrong origin", ">ATCSCC<", ">OTHER<", ErrInvalidQuery},
		{"wrong period kind", "GS CNX PERIOD", "GDP CNX PERIOD", ErrInvalidQuery},
		{"invalid ADL time", "ADL TIME: 1151Z", "ADL TIME: 2451Z", ErrInvalidQuery},
		{"duplicate period", "</c:advisoryText>", "\nGDP CNX PERIOD: 03/1151Z - 03/1323Z</c:advisoryText>", ErrInvalidQuery},
		{"period only in comments", "GS CNX PERIOD", "COMMENTS: NONE\nGS CNX PERIOD", ErrInvalidQuery},
		{"invalid time", "03/1323Z", "03/2423Z", ErrInvalidQuery},
		{"unresolved date", "03/1323Z", "29/1323Z", ErrInvalidQuery},
		{"reversed period", "03/1323Z", "03/1100Z", ErrInvalidQuery},
		{"wrong namespace", "c:advisoryTitle", "t:advisoryTitle", ErrInvalidQuery},
		{"duplicate title", "</c:advisoryTitle>", "</c:advisoryTitle><c:advisoryTitle>Other</c:advisoryTitle>", ErrInvalidQuery},
		{"oversized body", body, body + "\nCOMMENTS: " + strings.Repeat("a", maxTFMSGADVText), ErrInvalidQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.ReplaceAll(valid, tc.from, tc.to)
			got, err := TFMSAdvisoryBodies(strings.NewReader(doc))
			if len(got) != 0 || !errors.Is(err, tc.want) {
				t.Fatalf("converter returned %d bodies, error = %v, want %v", len(got), err, tc.want)
			}
		})
	}
	largeUnrelated := tfmsCancellationFixture("0059", "ATCSCC", "2026-09-03T11:56:17Z", "UNRELATED", strings.Repeat("a", maxTFMSGADVText+1), "")
	if got, err := TFMSAdvisoryBodies(strings.NewReader(largeUnrelated)); len(got) != 0 || !errors.Is(err, ErrAdvisoryIgnored) {
		t.Fatalf("large unrelated message returned %d bodies, error = %v", len(got), err)
	}
}
