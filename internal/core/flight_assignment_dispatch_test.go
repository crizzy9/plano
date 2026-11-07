package core

import (
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestFlightAlertDispatcherDeliversCurrentAircraftAndArrivalGateAssignments(t *testing.T) {
	for _, tc := range []struct {
		name, kind, assignedTitle, changedTitle string
		values                                  [3]string
		set                                     func(*feeds.Flight, string)
	}{
		{
			name: "aircraft", kind: "tail", assignedTitle: "Aircraft assigned", changedTitle: "Aircraft changed",
			values: [3]string{"N123AB", "N234AB", "N345AB"},
			set:    func(flight *feeds.Flight, value string) { flight.TailNumber = value },
		},
		{
			name: "arrival gate", kind: "arrival_gate", assignedTitle: "Arrival gate", changedTitle: "Arrival gate changed",
			values: [3]string{"C2", "C3", "C82"},
			set:    func(flight *feeds.Flight, value string) { flight.ArrivalGate = value },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, flight := flightAlertDispatchFixture(t)
			tc.set(&flight, tc.values[0])
			observeFlightAlertFixture(t, store, &flight)
			assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2), tc.assignedTitle+"|AA2 · "+tc.values[0])

			// Claim the intermediate assignment before its replacement arrives.
			// Delivery must discard it and submit the current assignment instead.
			tc.set(&flight, tc.values[1])
			observeFlightAlertFixture(t, store, &flight)
			obsolete := claimFlightAlertJobs(t, store, 2)
			tc.set(&flight, tc.values[2])
			observeFlightAlertFixture(t, store, &flight)
			assertFlightAlertSends(t, store, obsolete)
			assertFlightAlertSends(t, store, claimFlightAlertJobs(t, store, 2), tc.changedTitle+"|AA2 · "+tc.values[2])
			claimFlightAlertJobs(t, store, 0)
			assertFlightAlertHistory(t, store, flight.ID,
				tc.kind+"|AA2 · "+tc.values[0], tc.kind+"|AA2 · "+tc.values[1], tc.kind+"|AA2 · "+tc.values[2])
		})
	}
}
