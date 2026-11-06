package api

import (
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/feeds"
)

func TestYYZConnectionLocation(t *testing.T) {
	for _, tc := range []struct {
		name, terminal, gate, wantTerminal, wantSectors string
	}{
		{"terminal only", " Terminal 1 ", "", "1", ""},
		{"gate only", "", "gate D 38", "1", "D"},
		{"terminal 1 domestic swing", "T 1", "Gate D53 / F53", "1", "DF"},
		{"terminal 1 international swing", "", "F66/E66", "1", "EF"},
		{"terminal 3 swing", "t3", "A17/B17", "3", "AB"},
		{"terminal 3 domestic or international", "3", "C31", "3", "C"},
		{"terminal 3 gate suffix", "", "B2a", "3", "B"},
		{"terminal 3 US", "", "A7", "3", "A"},
		{"terminal conflict", "T3", "D38", "", ""},
		{"terminal 2", "2", "D38", "", ""},
		{"invalid explicit terminal", "Terminal", "D38", "", ""},
		{"unrecognized sector", "T1", "G41", "", ""},
		{"unverified pair", "T1", "D38/F38", "", ""},
		{"two different gates", "T3", "A17/B18", "", ""},
		{"more than two gates", "T3", "A17/B17/B18", "", ""},
		{"empty", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			terminal, sectors := yyzConnectionLocation(tc.terminal, tc.gate)
			if terminal != tc.wantTerminal || sectors != tc.wantSectors {
				t.Fatalf("location = (%q, %q), want (%q, %q)", terminal, sectors, tc.wantTerminal, tc.wantSectors)
			}
		})
	}
}

func TestYYZConnectionDirections(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		arrival, departure         feeds.Flight
		landside, screening        bool
		border, secureSameTerminal bool
		want                       []string
	}{
		{
			name:               "within Canada at terminal 1",
			arrival:            feeds.Flight{OriginICAO: "CYVR", ArrivalTerminal: "1", ArrivalGate: "D38"},
			departure:          feeds.Flight{DestinationICAO: "CYYC", DepartureTerminal: "1", DepartureGate: "D53/F53"},
			secureSameTerminal: true,
			want:               []string{"Connections signs within Terminal 1", "whether your checked bags are transferred"},
		},
		{
			name:               "within Canada at terminal 3",
			arrival:            feeds.Flight{Origin: "CYUL", ArrivalGate: "C31"},
			departure:          feeds.Flight{Destination: "CYOW", DepartureGate: "B17/A17"},
			secureSameTerminal: true,
			want:               []string{"Connections signs within Terminal 3"},
		},
		{
			name:      "international to Canada",
			arrival:   feeds.Flight{OriginICAO: "EGLL", ArrivalTerminal: "1", ArrivalGate: "E74"},
			departure: feeds.Flight{DestinationICAO: "CYOW", DepartureTerminal: "1", DepartureGate: "D38"},
			screening: true, border: true,
			want: []string{"Canadian border control", "domestic gate", "whether your checked bags are transferred"},
		},
		{
			name:      "US to Canada",
			arrival:   feeds.Flight{OriginICAO: "KLGA", ArrivalTerminal: "3", ArrivalGate: "A7"},
			departure: feeds.Flight{DestinationICAO: "CYWG", DepartureTerminal: "3", DepartureGate: "B37"},
			screening: true, border: true,
			want: []string{"Connections signs within Terminal 3", "Canadian border control", "domestic gate"},
		},
		{
			name:      "same terminal US preclearance at a swing gate",
			arrival:   feeds.Flight{OriginICAO: "CYUL", ArrivalTerminal: "1"},
			departure: feeds.Flight{DestinationICAO: "KSFO", DepartureGate: "D51"},
			screening: true, border: true,
			want: []string{"F gates", "security and U.S. preclearance"},
		},
		{
			name:      "international to US in terminal 1",
			arrival:   feeds.Flight{OriginICAO: "EGLL", ArrivalGate: "E74"},
			departure: feeds.Flight{DestinationICAO: "KLGA", DepartureGate: "F53"},
			screening: true, border: true,
			want: []string{"Canadian border inspection if directed", "F gates", "security and U.S. preclearance"},
		},
		{
			name:      "terminal 3 public entrance to US screening",
			arrival:   feeds.Flight{OriginICAO: "CYWG", ArrivalGate: "B37"},
			departure: feeds.Flight{DestinationICAO: "KLGA", DepartureGate: "A17/B17"},
			landside:  true, screening: true, border: true,
			want: []string{"recheck bags if your airline directs you", "A gates to the public departures entrance", "security and U.S. preclearance"},
		},
		{
			name:      "terminal change and US preclearance",
			arrival:   feeds.Flight{OriginICAO: "CYVR", ArrivalTerminal: "1", ArrivalGate: "D38"},
			departure: feeds.Flight{DestinationICAO: "KLGA", DepartureTerminal: "3", DepartureGate: "A7"},
			landside:  true, screening: true, border: true,
			want: []string{"Terminal Link train from Terminal 1 to Terminal 3", "recheck bags", "A gates", "U.S. preclearance"},
		},
		{
			name:      "terminal change from an international flight",
			arrival:   feeds.Flight{OriginICAO: "EGLL", ArrivalTerminal: "3", ArrivalGate: "C31"},
			departure: feeds.Flight{DestinationICAO: "LFPG", DepartureTerminal: "1", DepartureGate: "E74"},
			landside:  true, screening: true, border: true,
			want: []string{"Arrivals through Canadian border control", "Terminal Link train from Terminal 3 to Terminal 1", "departure screening"},
		},
		{
			name:      "same terminal international onward flight",
			arrival:   feeds.Flight{OriginICAO: "EGLL", ArrivalTerminal: "3", ArrivalGate: "C31"},
			departure: feeds.Flight{DestinationICAO: "EHAM", DepartureTerminal: "3", DepartureGate: "B37"},
			screening: true,
			want:      []string{"Pearson's connection guide", "which border checks and transfer screening apply"},
		},
		{
			name:      "ambiguous gate and IATA-only geography",
			arrival:   feeds.Flight{Origin: "YVR", ArrivalGate: "B37"},
			departure: feeds.Flight{Destination: "LGA", DepartureGate: "A17/B17"},
			screening: true,
			want:      []string{"Pearson's connection guide", "which border checks and transfer screening apply"},
		},
		{
			name:      "conflicting terminal",
			arrival:   feeds.Flight{OriginICAO: "CYVR", ArrivalTerminal: "3", ArrivalGate: "D38"},
			departure: feeds.Flight{DestinationICAO: "KLGA", DepartureGate: "A7"},
		},
		{
			name:      "outbound sector contradicts destination",
			arrival:   feeds.Flight{OriginICAO: "CYVR", ArrivalTerminal: "1"},
			departure: feeds.Flight{DestinationICAO: "KLGA", DepartureTerminal: "1", DepartureGate: "D38"},
		},
		{
			name:      "inbound sector contradicts Canadian origin",
			arrival:   feeds.Flight{OriginICAO: "CYVR", ArrivalTerminal: "1", ArrivalGate: "E74"},
			departure: feeds.Flight{DestinationICAO: "CYOW", DepartureTerminal: "1", DepartureGate: "D38"},
		},
		{
			name:      "missing departure location",
			arrival:   feeds.Flight{OriginICAO: "CYVR", ArrivalGate: "D38"},
			departure: feeds.Flight{DestinationICAO: "KLGA"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := yyzConnectionDirections(tc.arrival, tc.departure)
			if got.landside != tc.landside || got.requiresScreening != tc.screening || got.requiresBorderControl != tc.border ||
				got.secureSameTerminal != tc.secureSameTerminal || got.transferMinutes != 0 || (len(got.steps) == 0) != (len(tc.want) == 0) {
				t.Fatalf("unexpected route: %+v", got)
			}
			steps := strings.Join(got.steps, " ")
			remaining := steps
			for _, want := range tc.want {
				_, after, found := strings.Cut(remaining, want)
				if !found {
					t.Fatalf("directions %q missing ordered instruction %q", steps, want)
				}
				remaining = after
			}
		})
	}
}
