package feeds

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func aeroRegistryFixture(t *testing.T) *FAARegistry {
	t.Helper()
	var data bytes.Buffer
	archive := zip.NewWriter(&data)
	for _, file := range []struct{ name, csv string }{
		{"MASTER.txt", "N-NUMBER,MFR MDL CODE,YEAR MFR,STATUS CODE,MODE S CODE HEX\n12AA,0123456,2010,V,A00001\n"},
		{"ACFTREF.txt", "CODE,MFR,MODEL\n0123456,BOEING,737-800\n"},
	} {
		out, err := archive.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(out, file.csv); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	registry, err := LoadFAARegistryZIP(bytes.NewReader(data.Bytes()), int64(data.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestAeroRegistryEnrichesLiveSearchGetAndScheduleResolution(t *testing.T) {
	now := instant(t, "2026-11-05T20:00:00Z")
	departure := now.Add(time.Hour)
	row := aeroFlight{ID: "UAL7-live", IdentIATA: "UA7", Registration: "N12AA", AircraftType: "B738",
		Origin: aeroAirport{ICAO: "KSFO"}, Destination: aeroAirport{ICAO: "KJFK"}, ScheduledOut: &departure}
	body, err := json.Marshal(struct {
		Flights []aeroFlight `json:"flights"`
	}{[]aeroFlight{row}})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/aeroapi/flights/UA7", "/aeroapi/flights/UAL7", "/aeroapi/flights/UAL7-live":
			return response(http.StatusOK, string(body))
		default:
			return nil, fmt.Errorf("unexpected path: %s", r.URL.Path)
		}
	})}
	base := NewAeroAPI("fixture-key", client)
	base.now = func() time.Time { return now }
	aero := base.WithFAARegistry(aeroRegistryFixture(t))
	rows, err := aero.Search(t.Context(), "UA7", now)
	if err != nil || len(rows) != 1 {
		t.Fatalf("search = %+v, %v", rows, err)
	}
	direct, err := aero.Get(t.Context(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := scheduleKey{"UA7", "UAL7", "KSFO", "KJFK", departure}
	resolved, err := aero.Get(t.Context(), key.id())
	if err != nil || resolved.ID != key.id() || resolved.AeroProviderID() != row.ID {
		t.Fatalf("schedule resolution = %+v, %v", resolved, err)
	}
	for _, flight := range []Flight{rows[0], direct, resolved} {
		if flight.AircraftType != "B738" || flight.AircraftModel != "BOEING 737-800" || flight.AircraftAge == nil || *flight.AircraftAge != 16 {
			t.Errorf("enriched flight = %+v", flight)
		}
	}
	plain, err := base.Get(t.Context(), row.ID)
	if err != nil || plain.AircraftModel != "" || plain.AircraftAge != nil {
		t.Fatalf("original source = %+v, %v", plain, err)
	}
}

func TestAeroRegistryRequiresCurrentAssignment(t *testing.T) {
	now := instant(t, "2026-11-05T20:00:00Z")
	aero := NewAeroAPI("fixture-key", nil).WithFAARegistry(aeroRegistryFixture(t))
	for _, tc := range []struct {
		name   string
		change func(*aeroFlight)
		want   bool
	}{
		{"current scheduled", func(*aeroFlight) {}, true},
		{"current takeoff with old timetable", func(f *aeroFlight) {
			f.ScheduledOut, f.ActualOff = new(now.Add(-72*time.Hour)), new(now.Add(-time.Hour))
		}, true},
		{"historical departure", func(f *aeroFlight) { f.ScheduledOut = new(now.Add(-72 * time.Hour)) }, false},
		{"distant schedule", func(f *aeroFlight) { f.ScheduledOut = new(now.Add(72 * time.Hour)) }, false},
		{"missing departure", func(f *aeroFlight) { f.ScheduledOut = nil }, false},
		{"arrived", func(f *aeroFlight) { f.ActualIn = new(now) }, false},
		{"cancelled", func(f *aeroFlight) { f.Cancelled = true }, false},
		{"diverted", func(f *aeroFlight) { f.Diverted = true }, false},
		{"withdrawn registration", func(f *aeroFlight) { f.Registration = "" }, false},
		{"unmatched registration", func(f *aeroFlight) { f.Registration = "N13AA" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := aeroFlight{Registration: "N12AA", ScheduledOut: new(now.Add(-time.Hour))}
			tc.change(&row)
			flight := aero.normalizeFlight(row, now)
			if (flight.AircraftAge != nil) != tc.want || (flight.AircraftModel != "") != tc.want {
				t.Fatalf("aircraft age = %v, model = %q, want enrichment = %t", flight.AircraftAge, flight.AircraftModel, tc.want)
			}
		})
	}
}

func TestAeroRegistryLabelsOnlyApplicableCanadianMetadata(t *testing.T) {
	now := instant(t, "2026-11-05T20:00:00Z")
	ccar := &Registry{
		key: strings.ToUpper,
		byTail: map[string]registryEntry{
			"C-FIVM": {manufactured: 2008, model: "777-333ER", source: CCARRegistrySource},
			"C-FNEW": {manufactured: 2011, model: "767-300", source: CCARRegistrySource, validFrom: now.Add(24 * time.Hour)},
			"C-FUNK": {address: "c01234", source: CCARRegistrySource},
			"CFABC":  {ambiguous: true},
		},
	}
	aero := NewAeroAPI("fixture-key", nil).WithAircraftRegistry(CombineAircraftRegistries(aeroRegistryFixture(t), ccar))
	for _, tc := range []struct {
		name, tail, model, source string
		when                      time.Duration
		age                       int
	}{
		{name: "current Canadian", tail: "c-fivm", model: "777-333ER", source: CCARRegistrySource, age: 18},
		{name: "current FAA", tail: "N12AA", model: "BOEING 737-800", age: 16},
		{name: "historic Canadian", tail: "C-FIVM", when: -72 * time.Hour},
		{name: "future registration", tail: "C-FNEW"},
		{name: "address-only Canadian", tail: "C-FUNK"},
		{name: "ambiguous packed mark", tail: "CFABC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flight := aero.normalizeFlight(aeroFlight{
				AircraftType: "B77W", Registration: tc.tail, ScheduledOut: new(now.Add(tc.when)),
			}, now)
			model, age := flight.AircraftModel, flight.AircraftAge
			if tc.source != "" {
				if flight.AircraftRegistry == nil || flight.AircraftRegistry.Source != tc.source || model != "" || age != nil {
					t.Fatalf("expected attributed Canadian metadata only in the nested field: %+v", flight)
				}
				model, age = flight.AircraftRegistry.Model, flight.AircraftRegistry.Age
			} else if flight.AircraftRegistry != nil {
				t.Fatalf("unexpected Canadian metadata: %+v", flight.AircraftRegistry)
			}
			if flight.AircraftType != "B77W" || model != tc.model {
				t.Fatalf("unexpected enrichment: type %q, model %q", flight.AircraftType, model)
			}
			if tc.age == 0 && age != nil || tc.age != 0 && (age == nil || *age != tc.age) {
				t.Fatalf("age = %v, want %d or absent when zero", age, tc.age)
			}
		})
	}
}
