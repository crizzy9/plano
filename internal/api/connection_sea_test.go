package api

import (
	"slices"
	"testing"
)

func TestSEAConnectionDirections(t *testing.T) {
	for _, tc := range []struct {
		name, arrival, departure string
		want                     []string
	}{
		{"main concourses", "A2", "C10B", []string{"Follow the walkways and signs to the C gates."}},
		{"same satellite", "S2", "S8", []string{"Follow signs within the S gates."}},
		{"south loop", "S1", "b4", []string{
			"Take the SEA Underground Blue Line from the S gates to the B gates.", seaShuttleFallback,
		}},
		{"north loop reversed", " c10b ", "N6", []string{
			"Take the SEA Underground Green Line from the C gates to the N gates.", seaShuttleFallback,
		}},
		{"south to north satellite", "S4", "N8", []string{
			"Take the SEA Underground Blue Line from the S gates to the A gates.",
			"Take the SEA Underground Yellow Line from the A gates to the D gates.",
			"Take the SEA Underground Green Line from the D gates to the N gates.",
			seaShuttleFallback,
		}},
		{"north to south satellite", "N8", "S4", []string{
			"Take the SEA Underground Green Line from the N gates to the D gates.",
			"Take the SEA Underground Yellow Line from the D gates to the A gates.",
			"Take the SEA Underground Blue Line from the A gates to the S gates.",
			seaShuttleFallback,
		}},
		{"walk after shuttle", "N7", "B3", []string{
			"Take the SEA Underground Green Line from the N gates to the D gates.",
			"Take the SEA Underground Yellow Line from the D gates to the A gates.",
			"Follow the walkways and signs to the B gates.",
			seaShuttleFallback,
		}},
		{"walk before shuttle", "B3", "N7", []string{
			"Follow the walkways and signs to the A gates.",
			"Take the SEA Underground Yellow Line from the A gates to the D gates.",
			"Take the SEA Underground Green Line from the D gates to the N gates.",
			seaShuttleFallback,
		}},
		{"missing gate", "", "S1", nil},
		{"unknown prefix", "A1", "X4", nil},
		{"zero gate", "A00", "D4", nil},
		{"too many digits", "A100", "D4", nil},
		{"too many suffixes", "C10BB", "N3", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := seaConnectionDirections(tc.arrival, tc.departure)
			if !slices.Equal(got, tc.want) || tc.want == nil && got != nil {
				t.Fatalf("directions = %q, want %q", got, tc.want)
			}
		})
	}
}
