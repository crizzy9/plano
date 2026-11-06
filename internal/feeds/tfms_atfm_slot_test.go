package feeds

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const tfmsSlotFixture = `<?xml version="1.0" encoding="UTF-8"?>
<svc:tfmDataService xmlns:svc="urn:us:gov:dot:faa:atm:tfm:tfmdataservice"
  xmlns:flight="urn:us:gov:dot:faa:atm:tfm:flightdata"
  xmlns:common="urn:us:gov:dot:faa:atm:tfm:flightdatacommonmessages"
  xmlns:core="urn:us:gov:dot:faa:atm:tfm:tfmdatacoreelements"
  xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <svc:fltdOutput>
    <flight:fltdMessage msgType="FlightControl" fdTrigger="TMI_UPDATE" sourceFacility="TFMS"
      sourceTimeStamp="2026-11-06T18:48:12.250-05:00" flightRef="123456" acid="TST101"
      depArpt="ORD" arrArpt="KPHL" sensitivity="A">
      <flight:ncsmFlightControl>
        <common:qualifiedAircraftId>
          <core:aircraftId>TST101</core:aircraftId>
          <core:computerId><core:facilityIdentifier>TFMS</core:facilityIdentifier></core:computerId>
          <core:igtd>2026-11-06T23:36:00Z</core:igtd>
          <core:departurePoint><core:airport>KORD</core:airport></core:departurePoint>
          <core:arrivalPoint><core:airport>PHL</core:airport></core:arrivalPoint>
        </common:qualifiedAircraftId>
        <common:etd timeValue="2026-11-07T00:20:00Z" etdType="PROPOSED" />
        <common:controlIndicator>CONTROL_ACTIVE</common:controlIndicator>
        <common:ncsmControlData>
          <common:ctd>2026-11-07T00:13:00Z</common:ctd>
          <common:cta>2026-11-07T01:35:00Z</common:cta>
          <common:controlProgramType>GDP</common:controlProgramType>
        </common:ncsmControlData>
      </flight:ncsmFlightControl>
    </flight:fltdMessage>
  </svc:fltdOutput>
</svc:tfmDataService>`

func tfmsSlotFixtureBinding() (TFMSATFMSlotBinding, time.Time) {
	now := time.Date(2026, time.November, 6, 23, 50, 0, 0, time.UTC)
	return TFMSATFMSlotBinding{
		Version: 1, ProviderFlightID: "TST101-fixture", OriginICAO: "KORD", DestinationICAO: "KPHL",
		FlightRef: "123456", AircraftID: "TST101", SourceOrigin: "ORD", SourceDestination: "PHL",
		ScheduledDeparture: now.Add(-15 * time.Minute), IGTD: now.Add(-14 * time.Minute),
	}, now
}

func TestDecodeTFMSATFMSlot(t *testing.T) {
	binding, now := tfmsSlotFixtureBinding()
	observed := time.Date(2026, time.November, 6, 23, 48, 12, 250_000_000, time.UTC)
	for _, tc := range []struct {
		name, message, status, reason string
	}{
		{"ground delay assignment", tfmsSlotFixture, ATFMAssigned, "Ground delay program"},
		{"airspace flow assignment", strings.Replace(tfmsSlotFixture, ">GDP<", ">AFP<", 1), ATFMAssigned, "Airspace flow program"},
		{"collaborative trajectory assignment", strings.Replace(tfmsSlotFixture, ">GDP<", ">CTOP<", 1), ATFMAssigned, "Collaborative trajectory options program"},
		{"withdrawal with retained CTD", strings.Replace(tfmsSlotFixture, "CONTROL_ACTIVE", "CONTROL_CANCELED", 1), ATFMWithdrawn, ""},
		{"withdrawal without CTD", strings.ReplaceAll(strings.Replace(tfmsSlotFixture, "CONTROL_ACTIVE", "CONTROL_CANCELLED", 1),
			"<common:ctd>2026-11-07T00:13:00Z</common:ctd>", ""), ATFMWithdrawn, ""},
		{"withdrawal with nil CTD", strings.ReplaceAll(strings.Replace(tfmsSlotFixture, "CONTROL_ACTIVE", "CONTROL_CANCELED", 1),
			"<common:ctd>2026-11-07T00:13:00Z</common:ctd>", `<common:ctd xsi:nil="true"/>`), ATFMWithdrawn, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, readAt := range []time.Time{now, now.Add(5 * time.Minute)} {
				got, err := DecodeTFMSATFMSlot(strings.NewReader(tc.message), binding, readAt)
				if err != nil || got.Version != 1 || got.Source != FAASWIM || got.ProviderFlightID != binding.ProviderFlightID ||
					got.OriginICAO != binding.OriginICAO || got.DestinationICAO != binding.DestinationICAO ||
					!got.ScheduledDeparture.Equal(binding.ScheduledDeparture) || !got.ObservedAt.Equal(observed) ||
					got.Status != tc.status || got.Reason != tc.reason {
					t.Fatalf("slot = %+v, error = %v", got, err)
				}
				if tc.status == ATFMAssigned && (got.TakeoffAt == nil || !got.TakeoffAt.Equal(now.Add(23*time.Minute))) ||
					tc.status == ATFMWithdrawn && got.TakeoffAt != nil {
					t.Fatalf("unexpected takeoff time %v", got.TakeoffAt)
				}
			}
		})
	}
}

func TestDecodeTFMSATFMSlotRejectsAmbiguity(t *testing.T) {
	binding, now := tfmsSlotFixtureBinding()
	for _, tc := range []struct {
		name, old, replacement string
		changeBinding          func(*TFMSATFMSlotBinding)
	}{
		{name: "unsupported binding version", changeBinding: func(b *TFMSATFMSlotBinding) { b.Version = 2 }},
		{name: "wrong flight reference", old: `flightRef="123456"`, replacement: `flightRef="123457"`},
		{name: "wrong header callsign", old: `acid="TST101"`, replacement: `acid="TST102"`},
		{name: "wrong identity callsign", old: ">TST101<", replacement: ">TST102<"},
		{name: "wrong initial departure", old: "2026-11-06T23:36:00Z", replacement: "2026-11-06T23:37:00Z"},
		{name: "wrong header origin", old: `depArpt="ORD"`, replacement: `depArpt="MDW"`},
		{name: "wrong identity destination", old: ">PHL<", replacement: ">PIT<"},
		{name: "contradictory four-letter airport", changeBinding: func(b *TFMSATFMSlotBinding) { b.SourceOrigin = "KMDW" }},
		{name: "placeholder origin", old: `depArpt="ORD"`, replacement: `depArpt="ZZZZ"`},
		{name: "placeholder binding", changeBinding: func(b *TFMSATFMSlotBinding) { b.OriginICAO = "AFIL" }},
		{name: "missing observation time", old: `sourceTimeStamp="2026-11-06T18:48:12.250-05:00"`},
		{name: "observation without timezone", old: "2026-11-06T18:48:12.250-05:00", replacement: "2026-11-06T23:48:12"},
		{name: "old observation", old: "2026-11-06T18:48:12.250-05:00", replacement: "2026-11-06T23:10:00Z"},
		{name: "future observation", old: "2026-11-06T18:48:12.250-05:00", replacement: "2026-11-06T23:51:00Z"},
		{name: "wrong source", old: `sourceFacility="TFMS"`, replacement: `sourceFacility="ETMS"`},
		{name: "wrong trigger", old: `fdTrigger="TMI_UPDATE"`, replacement: `fdTrigger="TMI_OTHER"`},
		{name: "wrong message type", old: `msgType="FlightControl"`, replacement: `msgType="FlightModify"`},
		{name: "missing sensitivity", old: `sensitivity="A"`},
		{name: "no estimated-departure fallback", old: "<common:ctd>2026-11-07T00:13:00Z</common:ctd>"},
		{name: "ground stop does not supply EDCT", old: ">GDP<", replacement: ">GS<"},
		{name: "unscoped withdrawal", old: `CONTROL_ACTIVE</common:controlIndicator>
        <common:ncsmControlData>
          <common:ctd>2026-11-07T00:13:00Z</common:ctd>
          <common:cta>2026-11-07T01:35:00Z</common:cta>
          <common:controlProgramType>GDP</common:controlProgramType>`, replacement: `CONTROL_CANCELED</common:controlIndicator>
        <common:ncsmControlData>`},
		{name: "unknown status", old: "CONTROL_ACTIVE", replacement: "CONTROL_PENDING"},
		{name: "spoofed scalar namespace", old: "<common:ctd>2026-11-07T00:13:00Z</common:ctd>", replacement: "<core:ctd>2026-11-07T00:13:00Z</core:ctd>"},
		{name: "spoofed header namespace", old: `flightRef="123456"`, replacement: `core:flightRef="123456"`},
		{name: "spoofed nested scalar", old: "<common:ctd>2026-11-07T00:13:00Z</common:ctd>", replacement: "<common:aux><common:ctd>2026-11-07T00:13:00Z</common:ctd></common:aux>"},
		{name: "unexpected nested text", old: "<core:aircraftId>TST101</core:aircraftId>", replacement: "<core:aircraftId><core:token>TST101</core:token></core:aircraftId>"},
		{name: "nil identity wrapper with children", old: "<common:qualifiedAircraftId>", replacement: `<common:qualifiedAircraftId xsi:nil="1">`},
		{name: "nil takeoff scalar with text", old: "<common:ctd>", replacement: `<common:ctd xsi:nil="true">`},
		{name: "active slot with nil takeoff scalar", old: "<common:ctd>2026-11-07T00:13:00Z</common:ctd>", replacement: `<common:ctd xsi:nil="true"/>`},
		{name: "duplicate slot time", old: "<common:ctd>2026-11-07T00:13:00Z</common:ctd>", replacement: "<common:ctd>2026-11-07T00:13:00Z</common:ctd><common:ctd>2026-11-07T00:14:00Z</common:ctd>"},
		{name: "duplicate slot header", old: `flightRef="123456"`, replacement: `flightRef="123456" flightRef="123457"`},
		{name: "second flight record", old: "</svc:fltdOutput>", replacement: "<flight:fltdMessage/></svc:fltdOutput>"},
		{name: "external entity", old: "<svc:tfmDataService", replacement: `<!DOCTYPE svc:tfmDataService SYSTEM "urn:example:unsafe"><svc:tfmDataService`},
		{name: "trailing XML", old: "</svc:tfmDataService>", replacement: "</svc:tfmDataService><other/>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := binding
			if tc.changeBinding != nil {
				tc.changeBinding(&candidate)
			}
			message := tfmsSlotFixture
			if tc.old != "" {
				if !strings.Contains(message, tc.old) {
					t.Fatal("invalid fixture replacement")
				}
				message = strings.Replace(message, tc.old, tc.replacement, 1)
			}
			if got, err := DecodeTFMSATFMSlot(strings.NewReader(message), candidate, now); !errors.Is(err, ErrInvalidQuery) || got != (ATFMSlot{}) {
				t.Fatalf("slot = %+v, error = %v", got, err)
			}
		})
	}
}

func TestDecodeTFMSATFMSlotSkipsRestricted(t *testing.T) {
	binding, now := tfmsSlotFixtureBinding()
	for _, sensitivity := range []string{`sensitivity="R"`, `sensitivity="A" sensReason="DR"`} {
		message := strings.Replace(tfmsSlotFixture, `sensitivity="A"`, sensitivity, 1)
		if got, err := DecodeTFMSATFMSlot(strings.NewReader(message), binding, now); !errors.Is(err, ErrNotFound) || got != (ATFMSlot{}) {
			t.Fatalf("slot = %+v, error = %v", got, err)
		}
	}
}

func TestDecodeTFMSATFMSlotBoundsInput(t *testing.T) {
	binding, now := tfmsSlotFixtureBinding()
	if _, err := DecodeTFMSATFMSlot(strings.NewReader(strings.Repeat(" ", MaxTFMSATFMSlotBytes)+tfmsSlotFixture), binding, now); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("oversize error = %v", err)
	}
	if _, err := DecodeTFMSATFMSlot(iotest.ErrReader(errors.New("private source details")), binding, now); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "private source details") {
		t.Fatalf("reader error = %v", err)
	}
}
