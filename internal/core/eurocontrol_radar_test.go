package core

import (
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestEurocontrolRadarIndependentPagingAndReplayFloor(t *testing.T) {
	s, flight, report, now := swimRadarFixture(t)
	ctx := t.Context()
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	owner := trackSWIMRadarFixture(t, s, flight.ID)
	read := func(reader string, swim, eurocontrol *RadarCursor) RadarPage {
		t.Helper()
		page, err := s.RadarPageWithTraffic(ctx, reader, flight.ID, nil, nil, nil, nil, 256, 256, swim, eurocontrol)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	recordEurocontrol := func(positions ...feeds.Position) {
		t.Helper()
		eurocontrol := report
		eurocontrol.Positions = positions
		if err := s.RecordEurocontrolPositions(ctx, eurocontrol, now); err != nil {
			t.Fatal(err)
		}
	}
	first := report.Positions[0]
	swimLatest, eurocontrolLatest := first, first
	swimLatest.At = first.At.Add(200 * time.Millisecond)
	eurocontrolLatest.At = first.At.Add(100 * time.Millisecond)
	report.Positions = []feeds.Position{first, swimLatest}
	recordSWIMRadarFixture(t, s, report, now)
	// This stream starts at the exact same time and its second fix predates
	// the first stream's watermark. Both reports must still be admitted.
	first.Source, eurocontrolLatest.Source = feeds.FAASWIM, feeds.FAASWIM
	recordEurocontrol(first, eurocontrolLatest)
	initial := read(owner, &RadarCursor{}, &RadarCursor{})
	if len(initial.SWIMPositions) != 2 || initial.SWIMRevision == nil ||
		len(initial.EurocontrolPositions) != 2 || initial.EurocontrolRevision == nil ||
		!initial.SWIMPositions[0].At.Equal(initial.EurocontrolPositions[0].At) ||
		initial.SWIMPositions[0].Source != feeds.FAASWIM || initial.EurocontrolPositions[0].Source != feeds.Eurocontrol {
		t.Fatalf("streams did not persist overlapping fixes independently: %+v", initial)
	}
	swimCursor := &RadarCursor{After: &swimLatest.At, Revision: initial.SWIMRevision}
	eurocontrolCursor := &RadarCursor{After: &first.At, Revision: initial.EurocontrolRevision}
	page := read(owner, swimCursor, eurocontrolCursor)
	if len(page.SWIMPositions) != 0 || len(page.EurocontrolPositions) != 1 || !page.EurocontrolPositions[0].At.Equal(eurocontrolLatest.At) {
		t.Fatalf("streams did not page independently: %+v", page)
	}
	if untracked := read("unrelated", &RadarCursor{}, &RadarCursor{}); untracked.SWIMRevision != nil ||
		untracked.EurocontrolRevision != nil || untracked.EurocontrolPositions == nil || len(untracked.EurocontrolPositions) != 0 {
		t.Fatalf("untracked reader received EUROCONTROL data: %+v", untracked)
	}

	flight.TailNumber = "N222AA"
	flight.ObservedAt = first.At.Add(-time.Second)
	if err := s.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Each stream must reject its own erased fixes. A fresh fix between the
	// two old watermarks is valid only for the stream with the older floor.
	between := first
	between.At = first.At.Add(150 * time.Millisecond)
	report.Positions = []feeds.Position{between, swimLatest}
	recordSWIMRadarFixture(t, s, report, now)
	recordEurocontrol(eurocontrolLatest, between)
	ahead := now.Add(time.Minute)
	eurocontrolCursor.After = &ahead
	reset := read(owner, swimCursor, eurocontrolCursor)
	if reset.SWIMRevision == nil || *reset.SWIMRevision != swimLatest.At.UnixNano() || len(reset.SWIMPositions) != 0 ||
		reset.EurocontrolRevision == nil || *reset.EurocontrolRevision != eurocontrolLatest.At.UnixNano() ||
		len(reset.EurocontrolPositions) != 1 || !reset.EurocontrolPositions[0].At.Equal(between.At) {
		t.Fatalf("scope reset or stream-specific replay protection failed: %+v", reset)
	}
}
