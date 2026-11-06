package feeds

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestATFMSlotDecodeAndExpiry(t *testing.T) {
	now := time.Date(2026, time.November, 6, 23, 55, 0, 0, time.UTC)
	takeoff := now.Add(7 * time.Minute)
	base := ATFMSlot{Version: 1, Source: FAASWIM, ProviderFlightID: "UAL88-fixture", OriginICAO: "KSFO",
		DestinationICAO: "KEWR", ScheduledDeparture: now, ObservedAt: now.Add(-time.Minute), Status: ATFMAssigned, TakeoffAt: &takeoff}
	for _, tc := range []struct {
		name string
		edit func(*ATFMSlot)
		want time.Time
	}{
		{"FAA takeoff window across midnight", func(*ATFMSlot) {}, takeoff.Add(5 * time.Minute)},
		{"EUROCONTROL takeoff window", func(s *ATFMSlot) { s.Source = Eurocontrol }, takeoff.Add(10 * time.Minute)},
		{"withdrawal", func(s *ATFMSlot) { s.Status, s.TakeoffAt = ATFMWithdrawn, nil }, base.ObservedAt.Add(30 * time.Minute)},
		{"report freshness", func(s *ATFMSlot) { s.TakeoffAt = new(now.Add(time.Hour)) }, base.ObservedAt.Add(30 * time.Minute)},
		{"assignment without a time", func(s *ATFMSlot) { s.TakeoffAt = nil }, time.Time{}},
		{"withdrawal with a time", func(s *ATFMSlot) { s.Status = ATFMWithdrawn }, time.Time{}},
		{"future report", func(s *ATFMSlot) { s.ObservedAt = now.Add(time.Second) }, time.Time{}},
		{"stale report", func(s *ATFMSlot) { s.ObservedAt = now.Add(-31 * time.Minute) }, time.Time{}},
		{"unscoped airport", func(s *ATFMSlot) { s.OriginICAO = "SFO" }, time.Time{}},
		{"unsafe display text", func(s *ATFMSlot) { s.Reason = "a\nb" }, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slot := base
			tc.edit(&slot)
			body, err := json.Marshal(slot)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeATFMSlot(bytes.NewReader(body), now)
			if tc.want.IsZero() {
				if !errors.Is(err, ErrInvalidQuery) {
					t.Fatalf("invalid slot returned %v", err)
				}
			} else if err != nil || !got.ValidUntil().Equal(tc.want) {
				t.Fatalf("slot expiry = %v (%v), want %v", got.ValidUntil(), err, tc.want)
			}
		})
	}
}
