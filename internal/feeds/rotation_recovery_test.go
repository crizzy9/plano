package feeds

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestRotationExplicitLookupInterruptionAllowsSuffixRecovery(t *testing.T) {
	const inbound = `[{"fa_flight_id":"INBOUND","inbound_fa_flight_id":"OLDER","registration":"N1AA","origin":{"code_icao":"KDEN"},"destination":{"code_icao":"KSFO"},"scheduled_out":"2026-11-05T18:00:00Z","scheduled_in":"2026-11-05T21:00:00Z"}]`
	for _, tc := range []struct {
		name, direct, source string
		status               int
		cause                error
		recoverable          bool
	}{
		{"explicit interruption", "INBOUND", "INBOUND", 503, ErrUnavailable, true},
		{"invalid explicit query", "INBOUND", "INBOUND", 400, ErrInvalidQuery, false},
		{"registration rows can contradict the suffix", "", "N1AA", 503, ErrUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, requests := rotationFixture(t, map[string]rotationReply{
				tc.source: {200, inbound},
				"OLDER":   {tc.status, ""},
			})
			got, err := api.Rotation(t.Context(), rotationTarget(t, tc.direct, "N1AA"))
			if !errors.Is(err, tc.cause) || RotationSuffixRecoverable(err) != tc.recoverable ||
				!slices.Equal(rotationIDs(got), []string{"INBOUND"}) || !slices.Equal(*requests, []string{tc.source, "OLDER"}) {
				t.Fatalf("rotation = %v, recoverable = %v, error = %v, requests = %v",
					rotationIDs(got), RotationSuffixRecoverable(err), err, *requests)
			}
		})
	}
}

func TestRestoreRotationSuffixRevalidatesSavedLegs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(prefix, saved []Flight)
		want   []string
	}{
		{"same prefix and compatible suffix", func([]Flight, []Flight) {}, []string{"INBOUND", "OLDER", "EARLIEST"}},
		{"prefix identity changed", func(_ []Flight, saved []Flight) { saved[0].ID = "DIFFERENT" }, []string{"INBOUND"}},
		{"prefix route changed", func(_ []Flight, saved []Flight) {
			saved[0].Origin, saved[0].OriginICAO = "LAX", "KLAX"
		}, []string{"INBOUND"}},
		{"known ICAO conflict", func(prefix, saved []Flight) {
			prefix[0].OriginICAO, saved[0].OriginICAO = "KDEN", "KLAX"
		}, []string{"INBOUND"}},
		{"prefix registration changed", func(_ []Flight, saved []Flight) { saved[0].TailNumber = "N2AA" }, []string{"INBOUND"}},
		{"fresh predecessor link changed", func(prefix, _ []Flight) { prefix[0].InboundID = "DIFFERENT" }, []string{"INBOUND"}},
		{"cached predecessor contradicts link", func(_ []Flight, saved []Flight) { saved[1].ID = "DIFFERENT" }, []string{"INBOUND"}},
		{"older leg is outside the window", func(_ []Flight, saved []Flight) {
			saved[1].ScheduledDeparture = new(saved[1].ScheduledDeparture.Add(-24 * time.Hour))
			saved[1].ScheduledArrival = new(saved[1].ScheduledArrival.Add(-24 * time.Hour))
		}, []string{"INBOUND"}},
		{"older leg lands after revised pushback", func(prefix, _ []Flight) {
			prefix[0].ActualDeparture = new(prefix[0].ScheduledDeparture.Add(-2 * time.Hour))
		}, []string{"INBOUND"}},
		{"duplicate ends the cached suffix", func(_ []Flight, saved []Flight) { saved[2].ID = "OLDER" }, []string{"INBOUND", "OLDER"}},
		{"cancelled leg ends the cached suffix", func(_ []Flight, saved []Flight) { saved[2].Status = "cancelled" }, []string{"INBOUND", "OLDER"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := rotationTarget(t, "INBOUND", "N1AA")
			departure := *target.ScheduledDeparture
			saved := []Flight{
				{ID: "INBOUND", InboundID: "OLDER", Origin: "DEN", OriginICAO: "KDEN", Destination: "SFO", DestinationICAO: "KSFO",
					Status: "arrived", TailNumber: "N1AA", ObservedAt: departure.Add(-2 * time.Hour),
					ScheduledDeparture: new(departure.Add(-3 * time.Hour)), ScheduledArrival: new(departure.Add(-time.Hour))},
				{ID: "OLDER", OriginICAO: "KSEA", Destination: "DEN", DestinationICAO: "KDEN", Status: "arrived", TailNumber: "N1AA",
					ObservedAt:         departure.Add(-2 * time.Hour),
					ScheduledDeparture: new(departure.Add(-6 * time.Hour)), ScheduledArrival: new(departure.Add(-4 * time.Hour))},
				{ID: "EARLIEST", OriginICAO: "KLAX", DestinationICAO: "KSEA", Status: "arrived", TailNumber: "N1AA",
					ObservedAt:         departure.Add(-3 * time.Hour),
					ScheduledDeparture: new(departure.Add(-9 * time.Hour)), ScheduledArrival: new(departure.Add(-7 * time.Hour))},
			}
			fresh := saved[0]
			fresh.ObservedAt = departure
			fresh.OriginICAO = "" // The same origin via its IATA code.
			prefix := make([]Flight, 1, len(saved))
			prefix[0] = fresh
			tc.change(prefix, saved)
			prefixBefore, savedBefore := slices.Clone(prefix[:cap(prefix)]), slices.Clone(saved)
			got := RestoreRotationSuffix(target, prefix, saved)
			if !slices.Equal(rotationIDs(got), tc.want) {
				t.Fatalf("rotation = %v, want %v", rotationIDs(got), tc.want)
			}
			if !slices.Equal(got[:len(prefix)], prefixBefore[:len(prefix)]) ||
				!slices.Equal(got[len(prefix):], savedBefore[len(prefix):len(got)]) ||
				!slices.Equal(prefix[:cap(prefix)], prefixBefore) || !slices.Equal(saved, savedBefore) {
				t.Fatal("recovery changed fresh or cached observations")
			}
		})
	}
}

func TestRestoreRotationSuffixKeepsExistingLegLimit(t *testing.T) {
	target := rotationTarget(t, "LEG0", "N1AA")
	departure := *target.ScheduledDeparture
	saved := make([]Flight, maxRotationLegs+1)
	airports := []string{"KSFO", "KDEN"}
	for i := range saved {
		legDeparture := departure.Add(-time.Duration(i+1) * time.Hour)
		saved[i] = Flight{ID: fmt.Sprintf("LEG%d", i), InboundID: fmt.Sprintf("LEG%d", i+1),
			OriginICAO: airports[(i+1)%len(airports)], DestinationICAO: airports[i%len(airports)],
			Status: "arrived", TailNumber: target.TailNumber, ObservedAt: departure,
			ScheduledDeparture: &legDeparture, ScheduledArrival: new(legDeparture.Add(30 * time.Minute))}
	}
	if got := RestoreRotationSuffix(target, saved[:1], saved); !slices.Equal(got, saved[:maxRotationLegs]) {
		t.Fatalf("recovered %d legs, want the first %d", len(got), maxRotationLegs)
	}
}
