package feeds

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestDecodeCAT062PositionsUTCBinding(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 30, 0, time.UTC)
	binding := cat062PositionFixtureBinding(now)
	beforeMidnight := now.Add(-31 * time.Second)
	first := cat062PositionFixtureRecord(beforeMidnight, []byte{0}, nil, nil)
	fractional := cat062PositionFixtureRecord(beforeMidnight.Add(time.Second/128), []byte{0}, nil, nil)
	nextDay := cat062PositionFixtureRecord(now.Add(-time.Second), []byte{0}, nil, nil)
	raw := cat062WireTestBlock(fractional, nextDay, first, first)
	report, err := DecodeCAT062Positions(bytes.NewReader(raw), binding, now)
	if err != nil || len(report.Positions) != 2 ||
		!report.Positions[0].At.Equal(beforeMidnight) || !report.Positions[1].At.Equal(beforeMidnight.Add(time.Second/128)) {
		t.Fatalf("CAT062 midnight report = %+v, %v", report, err)
	}
	if p := report.Positions[0]; p.Latitude != -45 || p.Longitude != 90 || p.Source != Eurocontrol {
		t.Fatalf("unexpected signed coordinates or source: %+v", p)
	}
	conflicting := bytes.Clone(first)
	conflicting[7] ^= 0x01 // First coordinate octet.
	raw = cat062WireTestBlock(first, conflicting)
	if _, err := DecodeCAT062Positions(bytes.NewReader(raw), binding, now); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("conflicting positions at the same instant returned %v", err)
	}
}

func TestDecodeCAT062PositionsRejectsIneligibleTracks(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 30, 0, time.UTC)
	binding := cat062PositionFixtureBinding(now)
	at := now.Add(-31 * time.Second)
	for _, tc := range []struct {
		name     string
		status   []byte
		aircraft []byte
		ref      []byte
	}{
		{name: "tentative", status: []byte{0x02}},
		{name: "simulated", status: []byte{0x01, 0x80}},
		{name: "ending", status: []byte{0x01, 0x40}},
		{name: "coasting", status: []byte{1, 1, 1, 0x80}},
		{name: "plan only", status: []byte{1, 1, 1, 1, 0x02}},
		{name: "surface", status: []byte{1, 1, 1, 1, 1, 0x10}},
		{name: "wrong address", aircraft: []byte{0x80, 0xa1, 0xb2, 0xc4}},
		{name: "ADS-B on ground", aircraft: []byte{0x81, 0x10, 0xa1, 0xb2, 0xc3, 0x02, 0}},
		{name: "Mode S on ground", aircraft: []byte{0x81, 0x20, 0xa1, 0xb2, 0xc3, 0x04, 0}},
		{name: "non ICAO address", ref: []byte{3, 0x10, 0x12}},
		{name: "duplicate address after TVS", ref: []byte{8, 0x30, 0, 0, 0, 0, 0x01, 0xc0}},
		{name: "extended coasting", ref: []byte{4, 0x10, 0x01, 0x06}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.status == nil {
				tc.status = []byte{0}
			}
			raw := cat062WireTestBlock(cat062PositionFixtureRecord(at, tc.status, tc.aircraft, tc.ref))
			if _, err := DecodeCAT062Positions(bytes.NewReader(raw), binding, now); !errors.Is(err, ErrNotFound) {
				t.Fatalf("ineligible track returned %v", err)
			}
		})
	}
	stale := cat062PositionFixtureRecord(now.Add(-6*time.Minute), []byte{0}, nil, nil)
	if _, err := DecodeCAT062Positions(bytes.NewReader(cat062WireTestBlock(stale)), binding, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale track returned %v", err)
	}
	for _, ref := range [][]byte{{2, 0x80}, {3, 0x10, 0x01}, {3, 0x11, 0x00}} {
		raw := cat062WireTestBlock(cat062PositionFixtureRecord(at, []byte{0}, nil, ref))
		if _, err := DecodeCAT062Positions(bytes.NewReader(raw), binding, now); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("truncated REF %x returned %v", ref, err)
		}
	}
}

func cat062PositionFixtureBinding(now time.Time) CAT062PositionBinding {
	sac, sic, track := uint8(0), uint8(17), uint16(0xabcd)
	return CAT062PositionBinding{
		Version: 1, Edition: "1.21", DisplayAllowed: true, ObservedAirborne: true,
		ProviderFlightID: "BAW1-fixture", OriginICAO: "EGLL", DestinationICAO: "KJFK",
		ScheduledDeparture: now.Add(-time.Hour), ActualTakeoff: now.Add(-45 * time.Minute),
		TrackDate: now.Add(-time.Hour).Format(time.DateOnly), SAC: &sac, SIC: &sic, TrackNumber: &track, AircraftAddress: "a1b2c3",
	}
}

func cat062PositionFixtureRecord(at time.Time, status, aircraft, ref []byte) []byte {
	fspec := []byte{0x99, 0x1c}
	if ref != nil {
		fspec = []byte{0x99, 0x1d, 0x01, 0x01, 0x04}
	}
	if aircraft == nil {
		aircraft = []byte{0x80, 0xa1, 0xb2, 0xc3}
	}
	ticks := uint32(at.Sub(at.Truncate(24*time.Hour)) / (time.Second / 128))
	record := append(fspec, 0, 17, byte(ticks>>16), byte(ticks>>8), byte(ticks))
	record = append(record, 0xff, 0x80, 0, 0, 0x01, 0, 0, 0) // 45° S, 90° E.
	record = append(record, aircraft...)
	record = append(record, 0xab, 0xcd)
	record = append(record, status...)
	return append(record, ref...)
}
