package feeds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type testTransport func(*http.Request) (*http.Response, error)

func (t testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }

func response(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func instant(t *testing.T, value string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAeroSearchUsesDepartureLocalDateAndUnknowns(t *testing.T) {
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == "" || r.URL.Path != "/aeroapi/flights/UA7" || r.Header.Get("x-apikey") != "fixture-key" || r.URL.Query().Get("ident_type") != "designator" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		return response(200, `{"flights":[
			{"fa_flight_id":"UAL7-1","ident_iata":"UA7","registration":null,"origin":{"code_icao":"KSFO","code_iata":"SFO","timezone":"America/Los_Angeles"},"destination":{"code_icao":"KJFK","code_iata":"JFK"},"scheduled_out":"2026-11-06T06:00:00Z","actual_out":"2026-11-06T06:08:00Z","actual_off":"2026-11-06T06:25:00Z","gate_origin":"G3","terminal_origin":"I","baggage_claim":null},
			{"fa_flight_id":"UAL7-2","ident_iata":"UA7","origin":{"code_icao":"KSFO","timezone":"America/Los_Angeles"},"scheduled_out":"2026-11-06T21:00:00Z"},
			{"fa_flight_id":"UAL7-3","ident_iata":"UA7","blocked":true,"origin":{"code_icao":"KSFO","timezone":"America/Los_Angeles"},"scheduled_out":"2026-11-06T06:00:00Z"}]}`)
	})}
	aero := NewAeroAPI("fixture-key", client)
	aero.now = func() time.Time { return instant(t, "2026-11-05T20:00:00Z") }
	rows, err := aero.Search(t.Context(), " ua 7 ", instant(t, "2026-11-05T00:00:00Z"))
	if err != nil || len(rows) != 1 {
		t.Fatalf("search = %+v, %v", rows, err)
	}
	got := rows[0]
	if got.ID != "UAL7-1" || got.Origin != "SFO" || got.OriginICAO != "KSFO" || got.Destination != "JFK" || got.Status != "airborne" || got.ActualTakeoff == nil || got.DepartureGate != "G3" || got.DepartureTerminal != "I" || got.TailNumber != "" || got.Baggage != "" {
		t.Fatalf("flight = %+v", got)
	}
}

func TestAeroTrackKeepsMeasuredPositionsAndUnits(t *testing.T) {
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/aeroapi/flights/UAL7-1/track" || r.URL.Query().Get("include_estimated_positions") != "false" {
			t.Errorf("unexpected track request: %s", r.URL)
		}
		return response(200, `{"positions":[
			{"timestamp":"2026-11-06T06:31:00Z","latitude":1,"longitude":2,"altitude":330,"altitude_change":"D","groundspeed":445,"heading":50,"update_type":"A"},
			{"timestamp":"2026-11-06T06:30:00Z","latitude":3,"longitude":4,"update_type":"Z"},
			{"timestamp":"2026-11-06T06:31:00Z","latitude":1,"longitude":2,"update_type":"A"},
			{"timestamp":"2026-11-06T06:32:00Z","latitude":5,"longitude":6,"altitude_change":"-"},
			{"timestamp":"2026-11-06T06:33:00Z","latitude":7,"longitude":8,"altitude_change":"C"},
			{"timestamp":"2026-11-06T06:29:00Z","latitude":0,"longitude":0,"altitude_change":"C","update_type":"P"},
			{"timestamp":"2026-11-06T07:00:00Z","latitude":7,"longitude":9,"update_type":"A"},
			{"timestamp":"2026-11-06T06:29:00Z","latitude":400,"longitude":0},null]}`)
	})}
	aero := NewAeroAPI("fixture-key", client)
	aero.now = func() time.Time { return instant(t, "2026-11-06T06:32:30Z") }
	got, err := aero.Track(t.Context(), "UAL7-1")
	if err != nil || len(got) != 4 || got[0].Kind != "radar" || got[1].Kind != "ads_b" || got[1].AltitudeFeet == nil || *got[1].AltitudeFeet != 33000 || got[0].AltitudeFeet != nil {
		t.Fatalf("track = %+v, %v", got, err)
	}
	for index, want := range []string{"", "descending", "level", "climbing"} {
		if got[index].VerticalTrend != want || got[index].VerticalRateFPM != nil {
			t.Errorf("position %d vertical motion = %+v, want %q with no rate", index, got[index], want)
		}
	}
}

func TestAeroAirportPreservesZeroCoordinates(t *testing.T) {
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/aeroapi/airports/KJFK" {
			t.Errorf("unexpected airport request: %s", r.URL)
		}
		return response(200, `{"airport_code":"KJFK","code_icao":"KJFK","code_iata":"JFK","latitude":0,"longitude":0,"name":"Fixture airport","timezone":"America/New_York"}`)
	})}
	aero := NewAeroAPI("fixture-key", client)
	got, err := aero.Airport(t.Context(), " kjfk ")
	if err != nil || got.ICAO != "KJFK" || got.IATA != "JFK" || got.TimeZone != "America/New_York" || got.Latitude == nil || *got.Latitude != 0 || got.Longitude == nil || *got.Longitude != 0 {
		t.Fatalf("airport = %+v, %v", got, err)
	}
	if _, err := aero.Airport(t.Context(), "../credentials"); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("path traversal error = %v", err)
	}
}

func TestAeroFutureScheduleResolvesToSameLocalIdentity(t *testing.T) {
	now := instant(t, "2026-11-05T20:00:00Z")
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/aeroapi/schedules/"):
			if r.URL.Query().Get("flight_number") != "7" || r.URL.Query().Get("airline") != "UA" {
				t.Errorf("unexpected schedule query: %s", r.URL)
			}
			return response(200, `{"scheduled":[{"fa_flight_id":null,"ident":"UAL7","ident_icao":"UAL7","ident_iata":"UA7","actual_ident_icao":"UAL7","origin_icao":"KSFO","origin_iata":"SFO","destination_icao":"KJFK","destination_iata":"JFK","scheduled_out":"2026-11-16T06:00:00Z","scheduled_in":"2026-11-16T11:45:00Z"}]}`)
		case r.URL.Path == "/aeroapi/airports/KSFO":
			return response(200, `{"airport_code":"KSFO","timezone":"America/Los_Angeles"}`)
		case r.URL.Path == "/aeroapi/flights/UAL7":
			return response(200, `{"flights":[{"fa_flight_id":"UAL7-live","ident_iata":"UA7","origin":{"code_icao":"KSFO","code_iata":"SFO"},"destination":{"code_icao":"KJFK","code_iata":"JFK"},"scheduled_out":"2026-11-16T06:30:00Z","scheduled_in":"2026-11-16T12:15:00Z","gate_origin":"D2"}]}`)
		default:
			return nil, fmt.Errorf("unexpected path: %s", r.URL.Path)
		}
	})}
	aero := NewAeroAPI("fixture-key", client)
	aero.now = func() time.Time { return now }
	rows, err := aero.Search(t.Context(), "UA7", instant(t, "2026-11-15T00:00:00Z"))
	if err != nil || len(rows) != 1 || !strings.HasPrefix(rows[0].ID, schedulePrefix) {
		t.Fatalf("schedule search = %+v, %v", rows, err)
	}
	now = instant(t, "2026-11-15T05:00:00Z")
	resolved, err := aero.Get(t.Context(), rows[0].ID)
	if err != nil || resolved.ID != rows[0].ID || resolved.providerID != "UAL7-live" || resolved.DepartureGate != "D2" {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
}

func TestRotationKeepsLateInboundAndRejectsWrongTail(t *testing.T) {
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/aeroapi/flights/INBOUND":
			return response(200, `{"flights":[{"fa_flight_id":"INBOUND","ident_iata":"UA8","registration":"N1AA","inbound_fa_flight_id":"OUTBOUND","origin":{"code_icao":"KDEN","code_iata":"DEN"},"destination":{"code_icao":"KSFO","code_iata":"SFO"},"scheduled_out":"2026-11-05T18:00:00Z","scheduled_in":"2026-11-05T21:00:00Z","estimated_in":"2026-11-05T23:00:00Z"}]}`)
		case "/aeroapi/flights/N1AA":
			return response(200, `{"flights":[{"fa_flight_id":"WRONG","ident_iata":"UA9","registration":"N2AA","origin":{"code_icao":"KORD"},"destination":{"code_icao":"KDEN"},"scheduled_out":"2026-11-05T14:00:00Z","scheduled_in":"2026-11-05T17:00:00Z"},{"fa_flight_id":"PREVIOUS","ident_iata":"UA10","registration":"N1AA","origin":{"code_icao":"KORD"},"destination":{"code_icao":"KDEN"},"scheduled_out":"2026-11-05T13:00:00Z","scheduled_in":"2026-11-05T16:00:00Z"}]}`)
		default:
			return nil, fmt.Errorf("unexpected path: %s", r.URL.Path)
		}
	})}
	aero := NewAeroAPI("fixture-key", client)
	aero.now = func() time.Time { return instant(t, "2026-11-05T20:00:00Z") }
	departure := instant(t, "2026-11-05T22:00:00Z")
	flight := Flight{ID: "OUTBOUND", InboundID: "INBOUND", TailNumber: "N1AA", OriginICAO: "KSFO", ScheduledDeparture: &departure}
	got, err := aero.Rotation(t.Context(), flight)
	if err != nil || len(got) != 2 || got[0].ID != "INBOUND" || !got[0].EstimatedArrival.After(departure) || got[1].ID != "PREVIOUS" {
		t.Fatalf("rotation = %+v, %v", got, err)
	}
}

const faaFixture = `<AIRPORT_STATUS_INFORMATION>
  <Update_Time>Thu Dec 31 23:55:00 2026 GMT</Update_Time>
  <Delay_type><Name>Ground Stops</Name><Ground_Stop_List>
    <Program><ARPT>SFO</ARPT><Reason>weather</Reason><End_Time>010030Z</End_Time></Program>
    <Program><ARPT>SFO</ARPT><Reason>weather</Reason><End_Time>010100Z</End_Time></Program>
  </Ground_Stop_List></Delay_type>
  <Delay_type><Ground_Delay_List><Ground_Delay><ARPT>JFK</ARPT><Reason>airport volume</Reason><Avg>1 hour and 24 minutes</Avg></Ground_Delay></Ground_Delay_List></Delay_type>
  <Delay_type><Arrival_Departure_Delay_List><Delay><ARPT>LAX</ARPT><Reason>taxiway congestion</Reason>
     <Arrival_Departure Type="Departure"><Min>16 minutes</Min></Arrival_Departure>
     <Arrival_Departure Type="Arrival"><Min>20 minutes</Min></Arrival_Departure>
  </Delay></Arrival_Departure_Delay_List></Delay_type>
  <Delay_type><Airport_Closure_List><Airport><ARPT>LAS</ARPT><Reason>!LAS 12/000 LAS AD AP CLSD TO NON SKED TRANSIENT GA ACFT EXC PPR 2612290000-2701030000</Reason><Start>Dec 29 at 00:00 UTC.</Start><Reopen>Jan 03 at 00:00 UTC.</Reopen></Airport></Airport_Closure_List></Delay_type>
  <Delay_type><Airspace_Flow_List><Airspace_Flow><CTL_Element>FCAA</CTL_Element><Reason>staffing</Reason><AFP_StartTime>202612312300</AFP_StartTime><AFP_EndTime>202701010100</AFP_EndTime><Avg>45 minutes</Avg></Airspace_Flow></Airspace_Flow_List></Delay_type>
  <Delay_type><CTOP_List><CTOP><Program_Name>FCAB</Program_Name><Reason>weather</Reason><CTOP_Start_Time>202612312300</CTOP_Start_Time><CTOP_End_Time>202701010100</CTOP_End_Time><Avg>50 minutes</Avg></CTOP></CTOP_List></Delay_type>
</AIRPORT_STATUS_INFORMATION>`

func TestFAANASNormalizesDirectionalAirportAndAreaAdvisories(t *testing.T) {
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != faaNASEndpoint || r.Header.Get("Accept") != "application/xml" {
			t.Errorf("unexpected FAA request: %s", r.URL)
		}
		return response(200, faaFixture)
	})}
	got, err := NewFAANAS(client).Advisories(t.Context())
	if err != nil || len(got) != 7 {
		t.Fatalf("FAA advisories = %+v, %v", got, err)
	}
	if stop := got[0]; stop.Kind != "ground_stop" || stop.Direction != "arrival" || stop.EndsAt == nil || !stop.EndsAt.Equal(instant(t, "2027-01-01T01:00:00Z")) {
		t.Fatalf("ground stop = %+v", stop)
	}
	if delay := got[1]; delay.Kind != "ground_delay" || delay.Direction != "arrival" || delay.DelayMinutes == nil || *delay.DelayMinutes != 84 {
		t.Fatalf("ground delay = %+v", delay)
	}
	if got[2].Kind != "departure_delay" || got[3].Kind != "arrival_delay" {
		t.Fatalf("directional delays = %+v", got[2:4])
	}
	if notice := got[4]; notice.Kind != "airport_restriction" || !strings.Contains(notice.Reason, "general aviation") || !strings.Contains(notice.RawReason, "EXC PPR") || notice.EndsAt == nil || notice.EndsAt.Year() != 2027 {
		t.Fatalf("closure notice = %+v", notice)
	}
	if got[5].Kind != "airspace_flow" || got[5].Area != "FCAA" || got[5].Airport != "" || got[6].Kind != "ctop" || got[6].Area != "FCAB" {
		t.Fatalf("area advisories = %+v", got[5:])
	}
	if _, err := parseFAANAS([]byte(`<error/>`)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("malformed feed error = %v", err)
	}
}

func TestFAANASRetainsFullAirportClosureScopeAndPublishedEnd(t *testing.T) {
	fixture := strings.Replace(faaFixture,
		"AD AP CLSD TO NON SKED TRANSIENT GA ACFT EXC PPR 2612290000-2701030000",
		"AD AP CLSD 2612312200 - 2701010300EST", 1)
	rows, err := parseFAANAS([]byte(fixture))
	if err != nil || len(rows) != 7 {
		t.Fatalf("FAA advisories = %+v, %v", rows, err)
	}
	closure := rows[4]
	if closure.Kind != "airport_closure" || !strings.HasPrefix(closure.Reason, "Airport closed") ||
		closure.StartsAt == nil || !closure.StartsAt.Equal(instant(t, "2026-12-31T22:00:00Z")) ||
		closure.EndsAt == nil || !closure.EndsAt.Equal(instant(t, "2027-01-01T03:00:00Z")) {
		t.Fatalf("closure notice = %+v", closure)
	}
	if decoded, ok := DecodeNOTAM(closure.RawReason); !ok || decoded.Airport != "LAS" {
		t.Fatalf("affected airport missing from domestic notice: %+v", decoded)
	}
}

func TestFAANASPreservesDailyClosureScheduleAndExplicitDates(t *testing.T) {
	const raw = "!SNA 11/003 SNA AD AP CLSD DLY 0730-1415 2611030730-2611081415"
	start, end := instant(t, "2026-11-03T07:30:00Z"), instant(t, "2026-11-08T14:15:00Z")
	for _, tc := range []struct {
		name, reopen string
		wantEnd      time.Time
		compatible   bool
	}{
		{"raw dates only", "", end, true},
		{"matching reopen", "Nov 8 at 14:15 UTC.", end, true},
		{"conflicting reopen", "Nov 8 at 15:15 UTC.", end.Add(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := `<AIRPORT_STATUS_INFORMATION><Update_Time>Fri Nov 6 09:00:00 2026 UTC</Update_Time>
<Delay_type><Airport_Closure_List><Airport><ARPT>SNA</ARPT><Reason>` + raw + `</Reason>
<Reopen>` + tc.reopen + `</Reopen></Airport></Airport_Closure_List></Delay_type></AIRPORT_STATUS_INFORMATION>`
			rows, err := parseFAANAS([]byte(fixture))
			if err != nil || len(rows) != 1 {
				t.Fatalf("FAA advisories = %+v, %v", rows, err)
			}
			closure := rows[0]
			summary, ok := DecodeNOTAM(closure.RawReason)
			if !ok || !summary.HasDailyClosure() || summary.MatchesValidity(closure.StartsAt, closure.EndsAt) != tc.compatible ||
				closure.Kind != "airport_closure" || !strings.Contains(closure.Reason, "closed daily 0730-1415") ||
				closure.EndsAt == nil || !closure.EndsAt.Equal(tc.wantEnd) {
				t.Fatalf("daily closure notice = %+v", closure)
			}
			if tc.compatible && (closure.StartsAt == nil || !closure.StartsAt.Equal(start)) {
				t.Fatalf("published start was lost: %+v", closure)
			}
		})
	}
}

func TestAdvisoryGatewayBindsSourceAndRejectsUnusableRecords(t *testing.T) {
	body := `{"version":1,"advisories":[{"id":"nm-1","kind":"ground_delay","airport":"egll","observedAt":"2026-11-05T00:00:00Z","source":"untrusted-label","delayMinutes":25}]}`
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.URL.Path != "/advisories" {
			t.Errorf("unexpected gateway request: %s", r.URL)
		}
		return response(200, body)
	})}
	gateway, err := NewAdvisoryGateway("https://fixture.invalid/advisories", "fixture-token", Eurocontrol, client)
	if err != nil {
		t.Fatal(err)
	}
	got, err := gateway.Advisories(t.Context())
	if err != nil || len(got) != 1 || got[0].Source != Eurocontrol || got[0].Airport != "EGLL" {
		t.Fatalf("gateway = %+v, %v", got, err)
	}
	body = `{"version":1,"advisories":[{"id":"a","kind":"stop"}]}`
	if _, err := gateway.Advisories(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid snapshot error = %v", err)
	}
	if _, err := NewAdvisoryGateway("http://fixture.invalid/advisories", "fixture-token", FAASWIM, client); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("plaintext remote gateway error = %v", err)
	}
}

func TestFeedClientDoesNotForwardCredentialsOnRedirect(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://other.invalid/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	_, err := NewAeroAPI("fixture-key", client).Airport(context.Background(), "KSFO")
	if !errors.Is(err, ErrUnavailable) || calls != 1 {
		t.Fatalf("redirect followed or misclassified: calls=%d, err=%v", calls, err)
	}
}

func TestReadsbRejectsStalePrivateAndConflictingAircraft(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	body := `{"now":1800000000,"aircraft":[
		{"hex":"a00001","type":"adsb_icao","flight":"UAL7    ","r":"N12AA","lat":0,"lon":-122.1,"alt_baro":33000,"gs":445,"seen_pos":2},
		{"hex":"a00002","type":"adsb_icao","flight":"UAL8","r":"N13AA","lat":2,"lon":2,"seen_pos":2,"dbFlags":8},
		{"hex":"a00003","type":"adsb_icao","flight":"UAL9","r":"N14AA","lat":2,"lon":2,"seen_pos":50},
		{"hex":"a00004","type":"mlat","flight":"UAL10","r":"N15AA","lat":2,"lon":2,"seen_pos":0},
		{"hex":"a00005","type":"adsb_icao","flight":"UAL7","r":"N16AA","lat":2,"lon":2,"seen_pos":0},
		{"hex":"a00006","type":"adsb_icao","flight":"UAL7","r":"N16AA","lat":4,"lon":2,"seen_pos":0},
		{"hex":"a00007","type":"adsb_icao","flight":"UAL7","r":"N17AA","lat":2,"lon":2,"seen_pos":0,"alt_baro":"ground","alt_geom":650},
		{"hex":"a00008","type":"adsb_icao","flight":"UAL7","r":"N18AA","lat":2,"lon":2,"seen_pos":0,"ground":true,"alt_baro":675}]}`
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/data/aircraft.json" {
			t.Errorf("unexpected receiver request: %s", r.URL)
		}
		return response(200, body)
	})}
	receiver, err := NewADSBReceiver("https://fixture.invalid/data/aircraft.json", client)
	if err != nil {
		t.Fatal(err)
	}
	receiver.now = func() time.Time { return now }
	snapshot, err := receiver.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	flight := Flight{Status: "airborne", ICAONumber: "UAL7", TailNumber: "N12AA", ActualDeparture: new(now.Add(-time.Minute))}
	position, address, ok := snapshot.Match(flight, "A00001")
	if !ok || address != "a00001" || !position.At.Equal(now.Add(-2*time.Second)) || position.Latitude != 0 || position.Kind != "ads_b" || position.AltitudeFeet == nil || *position.AltitudeFeet != 33000 {
		t.Fatalf("position = %+v, address = %q, matched = %t", position, address, ok)
	}
	for _, tail := range []string{"N13AA", "N14AA", "N15AA", "N16AA", "N17AA", "N18AA"} {
		flight.TailNumber = tail
		if got, _, ok := snapshot.Match(flight, ""); ok {
			t.Errorf("accepted unsafe point for %s: %+v", tail, got)
		}
	}
	flight.TailNumber, flight.ICAONumber = "N12AA", "UAL81"
	if got, _, ok := snapshot.Match(flight, "A00001"); ok {
		t.Errorf("accepted conflicting callsign: %+v", got)
	}
	now = now.Add(61 * time.Second)
	if _, err := receiver.Snapshot(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale snapshot error = %v", err)
	}
}

func TestReadsbUsesReportedVerticalRate(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	snapshot, err := parseReadsb([]byte(`{"now":1800000000,"aircraft":[
		{"hex":"a00001","type":"adsb_icao","r":"N12AA","lat":0,"lon":1,"seen_pos":0,"baro_rate":0,"geom_rate":384,"track":20},
		{"hex":"a00002","type":"adsb_icao","r":"N13AA","lat":0,"lon":2,"seen_pos":0,"geom_rate":-512,"true_heading":0},
		{"hex":"a00003","type":"adsb_icao","r":"N14AA","lat":0,"lon":3,"seen_pos":0}]}`), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		tail string
		rate *float64
	}{
		{"N12AA", new(0.0)},
		{"N13AA", new(-512.0)},
		{"N14AA", nil},
	} {
		flight := Flight{Status: "airborne", TailNumber: test.tail, ActualTakeoff: new(now.Add(-time.Minute))}
		position, _, ok := snapshot.Match(flight, "")
		if !ok || position.VerticalTrend != "" || (position.VerticalRateFPM == nil) != (test.rate == nil) ||
			test.rate != nil && *position.VerticalRateFPM != *test.rate {
			t.Errorf("%s position = %+v, matched = %t", test.tail, position, ok)
		}
		if test.tail == "N12AA" && position.Heading != nil || test.tail == "N13AA" && (position.Heading == nil || *position.Heading != 0) {
			t.Errorf("%s heading = %v", test.tail, position.Heading)
		}
	}
}

func TestFAARegistryMatchesOnlyUsRegistrationAndKnownManufactureYear(t *testing.T) {
	registry, err := LoadFAARegistry(strings.NewReader("N-NUMBER,YEAR MFR,MODE S CODE HEX,NAME,\n12AA,2010,A00001,Ignored Name,\n13AA,,A00002,Ignored Name,\n14AA,2030,A00003,Ignored Name,\n15AA,2014,A00004,Ignored Name,\n15AA,2015,A00005,Ignored Name,\n"))
	if err != nil {
		t.Fatal(err)
	}
	at := instant(t, "2026-11-05T20:00:00Z")
	if age := registry.Age("n12aa", at); age == nil || *age != 16 || registry.ModeSHex("N12AA") != "a00001" {
		t.Fatalf("registry entry = age %v, address %q", age, registry.ModeSHex("N12AA"))
	}
	for _, tail := range []string{"N13AA", "N14AA", "N15AA", "EIABC"} {
		if age := registry.Age(tail, at); age != nil {
			t.Errorf("unexpected manufacture age for %s: %d", tail, *age)
		}
	}
	if registry.ModeSHex("N13AA") != "a00002" || registry.ModeSHex("N15AA") != "" {
		t.Fatal("missing manufacture year or ambiguous identity mishandled")
	}
}
