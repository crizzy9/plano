package feeds

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

const (
	tfmsFixtureObserved = "2026-11-06T14:13:21Z"
	tfmsFixtureStart    = "2026-11-06T15:00:00+00:00"
	tfmsFixtureEnd      = "2026-11-06T17:30:00Z"
	tfmsFixtureExpiry   = "2026-11-06T17:00:00Z"
)

func tfmsFixture(messageType, payload, attrs string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<ds:tfmDataService xmlns:ds="urn:us:gov:dot:faa:atm:tfm:tfmdataservice"
    xmlns:flow="urn:us:gov:dot:faa:atm:tfm:flowinformation"
    xmlns:c="urn:us:gov:dot:faa:atm:tfm:ficommonmessages"
    xmlns:t="urn:us:gov:dot:faa:atm:tfm:ficommondatatypes">
  <ds:fiOutput><flow:fiMessage msgType="%s" %s>%s</flow:fiMessage></ds:fiOutput>
</ds:tfmDataService>`, messageType, attrs, payload)
}

func tfmsFixtureProgram(messageType, status, updated, attrs string) string {
	var tag, period string
	switch messageType {
	case "GS":
		tag, period = "gsAdvisory", "groundStopPeriod"
	case "GDP":
		tag, period = "gdpAdvisory", "arrivalsEstimatedFor"
	}
	payload := fmt.Sprintf(`<flow:%[1]s>
    <c:updateTime>%[2]s</c:updateTime>
    <c:tmiStatus>%[3]s</c:tmiStatus>
    <c:airportId>ORD</c:airportId>
    <c:elementType>APT</c:elementType>
    <c:pgmExpTime>%[4]s</c:pgmExpTime>
    <c:%[5]s><t:startTime>%[6]s</t:startTime><t:endTime>%[7]s</t:endTime></c:%[5]s>
    <c:cumulativeProgramPeriod><t:startTime>2026-11-06T13:00:00Z</t:startTime><t:endTime>2026-11-06T19:00:00Z</t:endTime></c:cumulativeProgramPeriod>
    <c:impactingCondition>THUNDERSTORMS</c:impactingCondition>
    <c:avgDelay>46.0</c:avgDelay>
    <c:flightInclusionsUSList><t:flightInfo><t:flightId>UAL9987</t:flightId></t:flightInfo></c:flightInclusionsUSList>
</flow:%[1]s>`, tag, updated, status, tfmsFixtureExpiry, period, tfmsFixtureStart, tfmsFixtureEnd)
	return tfmsFixture(messageType, payload, attrs)
}

func TestTFMSAdvisoryBodiesAirportPrograms(t *testing.T) {
	for _, tc := range []struct {
		messageType, kind, label string
	}{
		{"GS", "ground_stop", "ground stop"},
		{"GDP", "ground_delay", "ground delay program"},
	} {
		t.Run(tc.messageType, func(t *testing.T) {
			active := tfmsFixtureProgram(tc.messageType, "ACTUAL", tfmsFixtureObserved, `sourceTimeStamp="2026-11-06T14:13:22Z"`)
			bodies, err := TFMSAdvisoryBodies(strings.NewReader(active))
			if err != nil || len(bodies) != 1 {
				t.Fatalf("converter returned %d bodies, error = %v", len(bodies), err)
			}
			got, err := DecodeAdvisoryIngress(bytes.NewReader(bodies[0]))
			if err != nil || got.Kind != tc.kind || got.Source != FAASWIM || got.Airport != "ORD" || got.Direction != "arrival" || !got.AirportContextOnly ||
				got.ID != "tfms/"+strings.ToLower(tc.messageType)+"/ORD" || got.DelayMinutes != nil || got.RawReason != "" || bytes.Contains(bodies[0], []byte("UAL9987")) {
				t.Fatalf("unexpected advisory = %+v, error = %v", got, err)
			}
			if got.ObservedAt.Format(time.RFC3339) != tfmsFixtureObserved || got.StartsAt == nil || got.StartsAt.Format(time.RFC3339) != "2026-11-06T15:00:00Z" ||
				got.EndsAt == nil || got.EndsAt.Format(time.RFC3339) != tfmsFixtureExpiry {
				t.Fatalf("unexpected advisory timing = %+v", got)
			}
			if want := "FAA SWIM reports a " + tc.label + " for selected flights to ORD. Reported cause is thunderstorms. Check with your airline about your flight."; got.Reason != want {
				t.Fatalf("advisory reason = %q, want %q", got.Reason, want)
			}

			refresh := tfmsFixtureProgram(tc.messageType, "ACTUAL", tfmsFixtureObserved, `refresh="RFRS" sourceTimeStamp="2026-11-06T14:30:00Z"`)
			refresh = strings.ReplaceAll(refresh, "UAL9987", "UNRELATED123")
			bodies2, err := TFMSAdvisoryBodies(strings.NewReader(refresh))
			if err != nil || len(bodies2) != 1 || !bytes.Equal(bodies[0], bodies2[0]) {
				t.Fatalf("refresh changed the advisory, error = %v", err)
			}

			const updateTime = "2026-11-06T14:38:07Z"
			update := tfmsFixtureProgram(tc.messageType, "ACTUAL", updateTime, "")
			update = strings.ReplaceAll(update, "THUNDERSTORMS", "EQUIPMENT")
			bodies3, err := TFMSAdvisoryBodies(strings.NewReader(update))
			if err != nil || len(bodies3) != 1 {
				t.Fatalf("updated converter returned %d bodies, error = %v", len(bodies3), err)
			}
			revised, err := DecodeAdvisoryIngress(bytes.NewReader(bodies3[0]))
			if err != nil || revised.ID != got.ID || revised.ObservedAt.Format(time.RFC3339) != updateTime || !strings.Contains(revised.Reason, "equipment") {
				t.Fatalf("unexpected advisory revision = %+v, error = %v", revised, err)
			}
		})
	}
}

func TestTFMSAdvisoryBodiesIgnoredAndMalformed(t *testing.T) {
	const airport = "<c:airportId>ORD</c:airportId>"
	active := tfmsFixtureProgram("GS", "ACTUAL", tfmsFixtureObserved, "")
	other := tfmsFixture("GADV", "<flow:generalAdvisory><c:advisoryText>Other program</c:advisoryText></flow:generalAdvisory>", "")
	for _, tc := range []struct {
		name, document string
		want           error
	}{
		{"proposed", tfmsFixtureProgram("GS", "PROPOSED", tfmsFixtureObserved, ""), ErrAdvisoryIgnored},
		{"unsupported", other, ErrAdvisoryIgnored},
		{"malformed unsupported", strings.Replace(other, "</flow:generalAdvisory>", "", 1), ErrInvalidQuery},
		{"wrong root namespace", strings.Replace(active, `xmlns:ds="urn:us:gov:dot:faa:atm:tfm:tfmdataservice"`, `xmlns:ds="urn:fixture:other"`, 1), ErrInvalidQuery},
		{"wrong payload", strings.ReplaceAll(active, "gsAdvisory", "gdpAdvisory"), ErrInvalidQuery},
		{"wrong field namespace", strings.Replace(active, airport, "<t:airportId>ORD</t:airportId>", 1), ErrInvalidQuery},
		{"wrong time namespace", strings.ReplaceAll(active, "t:endTime", "c:endTime"), ErrInvalidQuery},
		{"duplicate airport", strings.Replace(active, airport, airport+"<c:airportId>JFK</c:airportId>", 1), ErrInvalidQuery},
		{"nested field", strings.Replace(active, airport, "<c:airportId>O<t:inner>R</t:inner>D</c:airportId>", 1), ErrInvalidQuery},
		{"nested period text", strings.Replace(active, "<c:groundStopPeriod>", "<c:groundStopPeriod>invalid", 1), ErrInvalidQuery},
		{"missing revision time", strings.Replace(active, "<c:updateTime>"+tfmsFixtureObserved+"</c:updateTime>", "", 1), ErrInvalidQuery},
		{"ambiguous revision timezone", strings.Replace(active, tfmsFixtureObserved, "2026-11-06T14:13:21", 1), ErrInvalidQuery},
		{"invalid expiration", strings.Replace(active, tfmsFixtureExpiry, "unparseable", 1), ErrInvalidQuery},
		{"expiration before start", strings.Replace(active, tfmsFixtureExpiry, "2026-11-06T14:00:00Z", 1), ErrInvalidQuery},
		{"invalid XML directive", strings.Replace(active, "<ds:tfmDataService", "<!DOCTYPE tfmDataService><ds:tfmDataService", 1), ErrInvalidQuery},
		{"second document", active + other, ErrInvalidQuery},
		{"trailing data", active + "invalid", ErrInvalidQuery},
		{"oversized", strings.Repeat(" ", maxTFMSBytes+1), ErrInvalidQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := TFMSAdvisoryBodies(strings.NewReader(tc.document)); len(got) != 0 || !errors.Is(err, tc.want) {
				t.Fatalf("converter returned %d bodies, error = %v, want %v", len(got), err, tc.want)
			}
		})
	}
	if got, err := TFMSAdvisoryBodies(io.MultiReader(strings.NewReader(active), brokenTFMSReader{})); len(got) != 0 || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("converter returned %d bodies, error = %v, want %v", len(got), err, ErrUnavailable)
	}
}

type brokenTFMSReader struct{}

func (brokenTFMSReader) Read([]byte) (int, error) {
	return 0, errors.New("fixture read failure")
}
