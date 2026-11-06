package api

import (
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestPickupDoesNotReuseOriginalGateForDiversionOrCancellation(t *testing.T) {
	now := time.Now().UTC()
	arrival := now.Add(time.Hour)
	f := feeds.Flight{Number: "XY100", Origin: "LAX", Destination: "EWR", ArrivalGate: "C12", ArrivalTerminal: "C", Baggage: "4", ScheduledArrival: &arrival, ObservedAt: now}
	for _, status := range []string{"cancelled", "diverted"} {
		f.Status = status
		view := pickupView(f, now, false)
		if view.Arrival != "" || view.Flight.ArrivalGate != "" || view.Flight.ArrivalTerminal != "" || view.Flight.Baggage != "" {
			t.Fatalf("misleading pickup detail for %s: %#v", status, view)
		}
		if status == "diverted" && view.Notice == "" {
			t.Fatal("diversion missing context")
		}
	}
}
