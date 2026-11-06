package feeds

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const (
	sfdpsFixtureGUFI = "16727d08-157e-4d6b-8b35-162dd27cd918"
	sfdpsFixtureNS   = ` xmlns:nas="http://www.faa.aero/nas/3.0" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:fb="http://www.fixm.aero/base/3.0"`
)

// Layout adapted from the FIXM-NAS 3.0 report in OGC Testbed-16 Annex A.
// Values are synthetic and keep the receipt and radar target distinct.
func sfdpsFixtureFlight(gufi, observed string) string {
	return fmt.Sprintf(`<flight source="TH" timestamp="2026-11-06T16:49:00Z" xsi:type="nas:NasFlightType">
  <arrival arrivalPoint="KEWR"/>
  <departure departurePoint="KSFO"/>
  <enRoute>
    <position positionTime=%q reportSource="SURVEILLANCE" targetPositionTime="2026-11-06T16:46:00Z" xsi:type="nas:NasAircraftPositionType">
      <actualSpeed><surveillance uom="KNOTS">447.5</surveillance></actualSpeed>
      <altitude uom="FEET">35000</altitude>
      <position xsi:type="fb:LocationPointType"><location srsName="urn:ogc:def:crs:EPSG::4326"><pos>38.17 -87.45</pos></location></position>
      <targetAltitude uom="FEET">36000</targetAltitude>
      <targetPosition srsName="urn:ogc:def:crs:EPSG::4326"><pos>39.00 -86.00</pos></targetPosition>
    </position>
  </enRoute>
  <flightIdentification aircraftIdentification="UAL88"/>
  <flightStatus fdpsFlightStatus="ACTIVE"/>
  <gufi codeSpace="urn:uuid">%s</gufi>
</flight>`, observed, gufi)
}

func sfdpsFixtureCollection(flights ...string) string {
	var raw strings.Builder
	raw.WriteString(`<nas:MessageCollection` + sfdpsFixtureNS + `>`)
	for _, flight := range flights {
		raw.WriteString(`<message xsi:type="nas:FlightMessageType"><metadata/>`)
		raw.WriteString(flight)
		raw.WriteString(`</message>`)
	}
	raw.WriteString(`</nas:MessageCollection>`)
	return raw.String()
}

func sfdpsFixtureBinding() (SFDPSPositionBinding, time.Time) {
	now := time.Date(2026, 11, 6, 16, 50, 0, 0, time.UTC)
	return SFDPSPositionBinding{
		Version: 1, DisplayAllowed: true, ProviderFlightID: "UAL88-verified",
		OriginICAO: "KSFO", DestinationICAO: "KEWR", GUFI: sfdpsFixtureGUFI, AircraftID: "UAL88",
		ScheduledDeparture: now.Add(-2 * time.Hour), ActualTakeoff: now.Add(-time.Hour),
	}, now
}

func TestDecodeSFDPSPositionsSelectsBoundReportedFixes(t *testing.T) {
	binding, now := sfdpsFixtureBinding()
	recent := sfdpsFixtureFlight(sfdpsFixtureGUFI, "2026-11-06T11:48:18.125-05:00")
	earliest := sfdpsFixtureFlight(strings.ToUpper(sfdpsFixtureGUFI), "2026-11-06T16:45:00Z")
	unrelated := sfdpsFixtureFlight("7f582ce2-a2b5-428e-8c83-f82a0d6c65ab", "2026-11-06T16:49:30Z")
	plan := strings.Replace(recent, `source="TH"`, `source="FH"`, 1)
	raw := `<?xml version="1.0" encoding="UTF-8"?>` + sfdpsFixtureCollection(recent, unrelated, earliest, plan, recent)
	got, err := DecodeSFDPSPositions(strings.NewReader(raw), binding, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderFlightID != binding.ProviderFlightID || got.OriginICAO != binding.OriginICAO || got.DestinationICAO != binding.DestinationICAO ||
		!got.ScheduledDeparture.Equal(binding.ScheduledDeparture) || !got.ActualTakeoff.Equal(binding.ActualTakeoff) || len(got.Positions) != 2 {
		t.Fatalf("unexpected binding and fix count: %+v", got)
	}
	if !got.Positions[0].At.Equal(now.Add(-RadarPositionFreshness)) || !got.Positions[1].At.Equal(time.Date(2026, 11, 6, 16, 48, 18, 125000000, time.UTC)) {
		t.Fatalf("reported times or ordering lost: %v, %v", got.Positions[0].At, got.Positions[1].At)
	}
	for _, fix := range got.Positions {
		if fix.Latitude != 38.17 || fix.Longitude != -87.45 || fix.AltitudeFeet == nil || *fix.AltitudeFeet != 35000 ||
			fix.GroundSpeedKnots == nil || *fix.GroundSpeedKnots != 447.5 || fix.Source != FAASWIM || fix.Kind != "radar" {
			t.Fatalf("unexpected reported track fix: %+v", fix)
		}
	}
}

func TestDecodeSFDPSPositionsAcceptsQualifiedStandaloneMessage(t *testing.T) {
	binding, now := sfdpsFixtureBinding()
	binding.ActualTakeoff = now.Add(-time.Minute)
	flight := sfdpsFixtureFlight(sfdpsFixtureGUFI, "2026-11-06T16:49:00Z")
	flight = strings.Replace(flight, "38.17 -87.45", "0 0", 1)
	flight = strings.Replace(flight, `<altitude uom="FEET">35000</altitude>`, "", 1)
	flight = strings.Replace(flight, `<actualSpeed><surveillance uom="KNOTS">447.5</surveillance></actualSpeed>`, "", 1)
	// The spelling of a namespace prefix is immaterial. Its binding is not.
	raw := strings.ReplaceAll(`<nas:FlightMessage`+sfdpsFixtureNS+`>`+flight+`</nas:FlightMessage>`, "nas:", "f:")
	raw = strings.Replace(raw, "xmlns:nas=", "xmlns:f=", 1)
	got, err := DecodeSFDPSPositions(strings.NewReader(raw), binding, now)
	if err != nil || len(got.Positions) != 1 {
		t.Fatalf("expected one standalone fix, got %+v, error %v", got, err)
	}
	fix := got.Positions[0]
	if fix.Latitude != 0 || fix.Longitude != 0 || fix.AltitudeFeet != nil || fix.GroundSpeedKnots != nil || !fix.At.Equal(binding.ActualTakeoff) {
		t.Fatalf("optional or zero-valued observations changed: %+v", fix)
	}
}

func TestDecodeSFDPSPositionsSkipsNonObservedOrUnmatchedTracks(t *testing.T) {
	binding, now := sfdpsFixtureBinding()
	const stamp = "2026-11-06T16:48:18Z"
	flight := sfdpsFixtureFlight(sfdpsFixtureGUFI, stamp)
	for _, tc := range []struct{ name, old, replacement string }{
		{"unmatched GUFI", sfdpsFixtureGUFI, "7f582ce2-a2b5-428e-8c83-f82a0d6c65ab"},
		{"unmatched callsign", "UAL88", "UAL89"},
		{"unmatched route", "KEWR", "KJFK"},
		{"untrusted GUFI namespace", `codeSpace="urn:uuid"`, `codeSpace="urn:other"`},
		{"forecast message", `source="TH"`, `source="FH"`},
		{"ground phase", `fdpsFlightStatus="ACTIVE"`, `fdpsFlightStatus="PROPOSED"`},
		{"missing phase", `<flightStatus fdpsFlightStatus="ACTIVE"/>`, ""},
		{"manual report", `reportSource="SURVEILLANCE"`, `reportSource="MANUAL"`},
		{"coasting", `reportSource="SURVEILLANCE"`, `reportSource="SURVEILLANCE" coastIndicator="COASTING"`},
		{"missing reported time", `positionTime="` + stamp + `"`, ""},
		{"too old", stamp, "2026-11-06T16:44:59.999Z"},
		{"future", stamp, "2026-11-06T16:50:00.001Z"},
		{"only target position", `<position xsi:type="fb:LocationPointType"><location srsName="urn:ogc:def:crs:EPSG::4326"><pos>38.17 -87.45</pos></location></position>`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := sfdpsFixtureCollection(strings.Replace(flight, tc.old, tc.replacement, 1))
			got, err := DecodeSFDPSPositions(strings.NewReader(raw), binding, now)
			if !errors.Is(err, ErrNotFound) || len(got.Positions) != 0 {
				t.Fatalf("expected no usable fix, got %+v, error %v", got, err)
			}
		})
	}
	binding.ActualTakeoff = now.Add(-time.Minute)
	if _, err := DecodeSFDPSPositions(strings.NewReader(sfdpsFixtureCollection(flight)), binding, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("accepted a fix before the verified takeoff: %v", err)
	}
}

func TestDecodeSFDPSPositionsRejectsAmbiguityAndMalformedInput(t *testing.T) {
	binding, now := sfdpsFixtureBinding()
	flight := sfdpsFixtureFlight(sfdpsFixtureGUFI, "2026-11-06T16:48:18Z")
	raw := sfdpsFixtureCollection(flight)
	for _, tc := range []struct{ name, raw string }{
		{"unqualified root", strings.ReplaceAll(raw, "nas:MessageCollection", "MessageCollection")},
		{"lookalike root", strings.Replace(raw, sfdpsNAS, "urn:fake:nas", 1)},
		{"undeclared QName", strings.Replace(raw, "nas:NasFlightType", "other:NasFlightType", 1)},
		{"shadowed QName", strings.Replace(raw, `<flight source=`, `<flight xmlns:nas="urn:fake:nas" source=`, 1)},
		{"empty prefixed namespace", strings.ReplaceAll(strings.Replace(raw, `<flight source=`, `<p:flight xmlns:p="" source=`, 1), `</flight>`, `</p:flight>`)},
		{"pseudo namespace declaration", strings.Replace(strings.Replace(raw, `<flight source=`, `<flight xmlns:p="xmlns" p:bogus="`+sfdpsNAS+`" source=`, 1), `nas:NasFlightType`, `bogus:NasFlightType`, 1)},
		{"reserved XML prefix rebound", strings.Replace(raw, `<flight source=`, `<flight xmlns:xml="urn:fake:xml" source=`, 1)},
		{"reserved default namespace", strings.Replace(raw, `<flight source=`, `<flight xmlns="http://www.w3.org/2000/xmlns/" source=`, 1)},
		{"foreign selected element", strings.Replace(raw, "<enRoute>", `<enRoute xmlns="urn:fake:nas">`, 1)},
		{"unexpected en-route type", strings.Replace(raw, "<enRoute>", `<enRoute xsi:type="nas:NasFlightType">`, 1)},
		{"unexpected track type", strings.Replace(raw, "nas:NasAircraftPositionType", "nas:NasFlightType", 1)},
		{"non-point geometry type", strings.Replace(raw, "fb:LocationPointType", "fb:LocationLineType", 1)},
		{"duplicated reported coordinates", strings.Replace(raw, "</location>", "<pos>40 -80</pos></location>", 1)},
		{"duplicated attribute", strings.Replace(raw, `source="TH"`, `source="TH" source="FH"`, 1)},
		{"nested coordinates", strings.Replace(raw, "38.17 -87.45", "<value>38.17 -87.45</value>", 1)},
		{"conflicting same-time observations", sfdpsFixtureCollection(flight, strings.Replace(flight, "38.17 -87.45", "39.17 -88.45", 1))},
		{"wrong CRS", strings.Replace(raw, sfdpsWGS84, "urn:ogc:def:crs:OGC::CRS84", 1)},
		{"wrong units", strings.Replace(raw, `uom="FEET"`, `uom="METERS"`, 1)},
		{"non-finite coordinates", strings.Replace(raw, "38.17 -87.45", "NaN -87.45", 1)},
		{"out-of-range latitude", strings.Replace(raw, "38.17 -87.45", "180 -87.45", 1)},
		{"time without zone", strings.Replace(raw, "2026-11-06T16:48:18Z", "2026-11-06T16:48:18", 1)},
		{"entity declaration", `<!DOCTYPE input [<!ENTITY x "38.17 -87.45">]>` + raw},
		{"trailing XML", raw + raw},
		{"truncated", strings.TrimSuffix(raw, `</nas:MessageCollection>`)},
		{"oversized", strings.Repeat(" ", maxSFDPSPositionBytes) + raw},
		{"depth limit", strings.Replace(raw, "<metadata/>", strings.Repeat("<other>", maxSFDPSDepth)+strings.Repeat("</other>", maxSFDPSDepth), 1)},
		{"record limit", sfdpsFixtureCollection(slicesOfSFDPSFlights(flight, maxSFDPSMessages+1)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeSFDPSPositions(strings.NewReader(tc.raw), binding, now)
			if !errors.Is(err, ErrInvalidQuery) || len(got.Positions) != 0 {
				t.Fatalf("expected rejected document, got %+v, error %v", got, err)
			}
		})
	}
	if _, err := DecodeSFDPSPositions(iotest.ErrReader(ioErrForSFDPSFixture), binding, now); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("input read failure was not retryable: %v", err)
	}
	binding.DisplayAllowed = false
	if _, err := DecodeSFDPSPositions(strings.NewReader(raw), binding, now); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("accepted display without permission: %v", err)
	}
}

var ioErrForSFDPSFixture = errors.New("input lost")

func slicesOfSFDPSFlights(flight string, count int) []string {
	flights := make([]string, count)
	for i := range flights {
		flights[i] = flight
	}
	return flights
}
