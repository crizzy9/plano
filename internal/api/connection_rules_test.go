package api

import (
	"slices"
	"strings"
	"testing"
)

const connectionFixture = `{
  "version": 1,
  "airports": [
    {
      "codes": ["XZZ", "KXZZ"],
      "mapUrl": "https://airport.example/terminal-maps",
      "bufferMinutes": 15,
      "nodes": [
        {"id":"a1","label":"gate A1","terminal":"A","gate":"A1"},
        {"id":"b1","label":"gate B1","terminal":"B","gate":"B1"},
        {"id":"a","label":"terminal A connections","terminal":"A"},
        {"id":"b","label":"terminal B connections","terminal":"B"},
        {"id":"security","label":"security checkpoint"},
        {"id":"a9","label":"gate A9","terminal":"A","gate":"9"},
        {"id":"b9","label":"gate B9","terminal":"B","gate":"9"},
        {"id":"a10","label":"gate A10","terminal":"A","gate":"A10"}
      ],
      "edges": [
        {"from":"a1","to":"b1","minutes":34},
        {"from":"a1","to":"security","minutes":6,"instruction":"Follow signs to transfer security."},
        {"from":"security","to":"b1","minutes":9},
        {"from":"a","to":"b","minutes":26},
        {"from":"a","to":"b1","minutes":29},
        {"from":"a9","to":"b1","minutes":20},
        {"from":"b9","to":"b1","minutes":4}
      ]
    },
    {"codes":["XZV"],"mapUrl":"https://other-airport.example/map"}
  ]
}`

func TestConnectionRulesFindShortestDirectedPath(t *testing.T) {
	rules, err := LoadConnectionRules(strings.NewReader(connectionFixture))
	if err != nil {
		t.Fatal(err)
	}
	route, ok := rules.Route("xzz", "", " a ", "a1", "b", "b1")
	if !ok || route.TransferMinutes != 30 || route.Precision != "gates" || route.MapURL != "https://airport.example/terminal-maps" {
		t.Fatalf("unexpected configured route %+v, ok=%v", route, ok)
	}
	want := []string{"Follow signs to transfer security.", "Continue to gate B1."}
	if !slices.Equal(route.Instructions, want) {
		t.Fatalf("instructions = %q, want %q", route.Instructions, want)
	}
	reverse, ok := rules.Route("", "KXZZ", "B", "B1", "A", "A1")
	if ok || reverse.TransferMinutes != 0 || reverse.MapURL != route.MapURL {
		t.Fatalf("one-way route reversed: %+v, ok=%v", reverse, ok)
	}
}

func TestConnectionRulesEndpointPrecisionAndFallback(t *testing.T) {
	rules, err := LoadConnectionRules(strings.NewReader(connectionFixture))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, airport, alternate, inTerminal, inGate, outTerminal, outGate, precision string
		minutes                                                                       int
		wantMap                                                                       bool
	}{
		{name: "terminal path", airport: "XZZ", inTerminal: "A", inGate: "UNKNOWN", outTerminal: "B", precision: "terminals", minutes: 41, wantMap: true},
		{name: "mixed path", alternate: "KXZZ", inTerminal: "A", outTerminal: "B", outGate: "B1", precision: "mixed", minutes: 44, wantMap: true},
		{name: "unique gate without terminal", airport: "XZZ", inGate: "A1", outGate: "B1", precision: "gates", minutes: 30, wantMap: true},
		{name: "ambiguous gate without terminal", airport: "XZZ", inGate: "9", outGate: "B1", wantMap: true},
		{name: "gate qualified by terminal", airport: "XZZ", inTerminal: "B", inGate: "9", outGate: "B1", precision: "gates", minutes: 19, wantMap: true},
		{name: "same gate retains buffer", airport: "XZZ", inGate: "A1", outGate: "A1", precision: "gates", minutes: 15, wantMap: true},
		{name: "same terminal alone needs generic estimate", airport: "XZZ", inTerminal: "A", outTerminal: "A", wantMap: true},
		{name: "disconnected exact gate needs generic estimate", airport: "XZZ", inTerminal: "A", inGate: "A10", outTerminal: "B", outGate: "B1", wantMap: true},
		{name: "airport map without graph", airport: "XZV", inTerminal: "A", outTerminal: "B", wantMap: true},
		{name: "unconfigured airport", airport: "XZY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route, ok := rules.Route(tc.airport, tc.alternate, tc.inTerminal, tc.inGate, tc.outTerminal, tc.outGate)
			if ok != (tc.precision != "") || route.Precision != tc.precision ||
				route.TransferMinutes != tc.minutes || (route.MapURL != "") != tc.wantMap || (len(route.Instructions) > 0) != ok {
				t.Fatalf("unexpected route %+v, ok=%v", route, ok)
			}
		})
	}
	var unconfigured *ConnectionRules
	if route, ok := unconfigured.Route("XZZ", "", "A", "", "B", ""); ok || route.MapURL != "" {
		t.Fatalf("unexpected nil configuration result %+v, ok=%v", route, ok)
	}
}

func TestConnectionRulesRejectInvalidConfiguration(t *testing.T) {
	invalid := map[string]string{
		"unsupported version":       strings.Replace(connectionFixture, `"version": 1`, `"version": 2`, 1),
		"extra document":            connectionFixture + "{}",
		"unknown field":             strings.Replace(connectionFixture, `"version": 1`, `"version": 1, "extra": true`, 1),
		"bad map scheme":            strings.Replace(connectionFixture, "https://airport.example", "http://airport.example", 1),
		"map with credentials":      strings.Replace(connectionFixture, "https://airport.example", "https://user:password@airport.example", 1),
		"case-normalized duplicate": strings.Replace(connectionFixture, `"KXZZ"`, `"xzz"`, 1),
		"duplicate gate":            strings.Replace(connectionFixture, `"gate":"A10"`, `"gate":"A1"`, 1),
		"undefined edge endpoint":   strings.Replace(connectionFixture, `"to":"security"`, `"to":"missing"`, 1),
		"invalid edge duration":     strings.Replace(connectionFixture, `"minutes":34`, `"minutes":0`, 1),
		"invalid buffer":            strings.Replace(connectionFixture, `"bufferMinutes": 15`, `"bufferMinutes": 121`, 1),
		"oversized file":            strings.Repeat(" ", maxConnectionRulesBytes+1),
	}
	for name, input := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConnectionRules(strings.NewReader(input)); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}
