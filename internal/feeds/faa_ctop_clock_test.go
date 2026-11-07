package feeds

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The parent clocks and FCA dates are published example values from
// https://www.fly.faa.gov/fly/What_s_New/XML_is_changing.
// Other fields are representative; this partial wire fixture omits FCA geometry.
const faaCTOPClockFixture = `<AIRPORT_STATUS_INFORMATION>
  <Update_Time>UPDATE_TIME</Update_Time>
  <Delay_type><CTOP_List><CTOP>
    <Program_Name>CTOP001</Program_Name><Reason>  WEATHER /  THUNDERSTORMS  </Reason>
    <CTOP_Start_Time>START_TIME</CTOP_Start_Time><CTOP_End_Time>END_TIME</CTOP_End_Time><Avg>50 minutes</Avg>
    <FCA_Data>
      <FCA><FCA_Name>FCA001</FCA_Name><FCA_Start_DateTime>20130127201500</FCA_Start_DateTime><FCA_End_DateTime>20130128024500</FCA_End_DateTime></FCA>
      <FCA><FCA_Name>FCA002</FCA_Name><FCA_Start_DateTime>20130127171500</FCA_Start_DateTime><FCA_End_DateTime>20130127220000</FCA_End_DateTime></FCA>
    </FCA_Data>
  </CTOP></CTOP_List></Delay_type>
</AIRPORT_STATUS_INFORMATION>`

func faaCTOPClockBody(updated, start, end string) string {
	return strings.NewReplacer("UPDATE_TIME", updated, "START_TIME", start, "END_TIME", end).Replace(faaCTOPClockFixture)
}

func TestFAANASPreservesCTOPClocksAcrossPollDates(t *testing.T) {
	var contextID string
	for _, tc := range []struct {
		updated, observed string
	}{
		{"Sun Jan 27 23:55:00 2013 GMT", "2013-01-27T23:55:00Z"},
		{"Mon Jan 28 00:05:00 2013 GMT", "2013-01-28T00:05:00Z"},
		{"Thu Feb 28 23:55:00 2013 GMT", "2013-02-28T23:55:00Z"},
		{"Fri Mar 1 00:05:00 2013 GMT", "2013-03-01T00:05:00Z"},
		{"Tue Dec 31 23:55:00 2013 GMT", "2013-12-31T23:55:00Z"},
		{"Wed Jan 1 00:05:00 2014 GMT", "2014-01-01T00:05:00Z"},
	} {
		t.Run(tc.observed, func(t *testing.T) {
			rows, err := parseFAANAS([]byte(faaCTOPClockBody(tc.updated, "  1735  ", "\n 0430 \n")))
			if err != nil || len(rows) != 1 {
				t.Fatalf("FAA CTOP advisories = %+v, error = %v", rows, err)
			}
			got := rows[0]
			if got.StartsAt != nil || got.EndsAt != nil || got.RawStartTime != "1735" || got.RawEndTime != "0430" {
				t.Fatalf("clock-only parent acquired dates or lost source clocks: %+v", got)
			}
			if got.Kind != "ctop" || got.Area != "CTOP001" || got.Airport != "" || got.Direction != "" ||
				got.Reason != "WEATHER /  THUNDERSTORMS" || got.RawReason != got.Reason {
				t.Fatalf("CTOP context or reason changed: %+v", got)
			}
			if got.Source != faaNASSource || got.ObservedAt.Format(time.RFC3339) != tc.observed {
				t.Fatalf("CTOP source provenance changed: %+v", got)
			}
			if contextID == "" {
				contextID = got.ID
			}
			if got.ID == "" || got.ID != contextID {
				t.Fatalf("poll date changed CTOP context ID: got %q, want %q", got.ID, contextID)
			}
		})
	}
}

func TestFAANASCTOPClocksSurviveJSONRoundTrip(t *testing.T) {
	rows, err := parseFAANAS([]byte(faaCTOPClockBody("Sun Jan 27 23:55:00 2013 GMT", "1735", "0430")))
	if err != nil || len(rows) != 1 {
		t.Fatalf("FAA CTOP advisories = %+v, error = %v", rows, err)
	}
	body, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["rawStartTime"]) != `"1735"` || string(fields["rawEndTime"]) != `"0430"` ||
		fields["startsAt"] != nil || fields["endsAt"] != nil {
		t.Fatalf("JSON lost source clocks or invented absolute boundaries: %s", body)
	}
	var got Advisory
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.RawStartTime != "1735" || got.RawEndTime != "0430" || got.StartsAt != nil || got.EndsAt != nil ||
		got.ID != rows[0].ID || got.Reason != rows[0].Reason || got.RawReason != rows[0].RawReason ||
		got.Source != faaNASSource || !got.ObservedAt.Equal(rows[0].ObservedAt) {
		t.Fatalf("JSON round trip changed CTOP context: %+v", got)
	}
}

func TestFAANASPreservesExplicitlyDatedCTOPTiming(t *testing.T) {
	for _, tc := range []struct {
		name, start, end, wantEnd, rawEnd string
	}{
		{"compact dates", "202612312300", "202701010100", "2027-01-01T01:00:00Z", ""},
		{"RFC3339 dates", "2026-12-31T18:00:00-05:00", "2027-01-01T01:00:00Z", "2027-01-01T01:00:00Z", ""},
		{"dated start with clock-only end", "202612312300", "0430", "", "0430"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := parseFAANAS([]byte(faaCTOPClockBody("Thu Dec 31 23:55:00 2026 GMT", tc.start, tc.end)))
			if err != nil || len(rows) != 1 {
				t.Fatalf("FAA CTOP advisories = %+v, error = %v", rows, err)
			}
			got := rows[0]
			if got.StartsAt == nil || !got.StartsAt.Equal(instant(t, "2026-12-31T23:00:00Z")) ||
				got.RawStartTime != "" || got.RawEndTime != tc.rawEnd {
				t.Fatalf("explicit CTOP start or raw clocks changed: %+v", got)
			}
			if tc.wantEnd == "" {
				if got.EndsAt != nil {
					t.Fatalf("clock-only end acquired a date: %+v", got)
				}
			} else if got.EndsAt == nil || !got.EndsAt.Equal(instant(t, tc.wantEnd)) {
				t.Fatalf("explicit CTOP end changed: %+v", got)
			}
		})
	}
}

func TestFAANASClockMetadataIsLimitedToCTOPHHMM(t *testing.T) {
	for _, tc := range []struct {
		name, start, end, wantStart, wantEnd string
		airspace                             bool
	}{
		{"clock limits", " 0000 ", " 2359 ", "0000", "2359", false},
		{"missing start", "", "0430", "", "0430", false},
		{"missing end", "1735", "", "1735", "", false},
		{"invalid clock", "2400", "1260", "", "", false},
		{"not four digits", "+100", "173", "", "", false},
		{"other notation", "17:35", "0430Z", "", "", false},
		{"airspace flow", "1735", "0430", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := faaCTOPClockBody("Sun Jan 27 23:55:00 2013 GMT", tc.start, tc.end)
			if tc.airspace {
				body = strings.NewReplacer("CTOP_List", "Airspace_Flow_List", "<CTOP>", "<Airspace_Flow>", "</CTOP>", "</Airspace_Flow>",
					"CTOP_Start_Time", "AFP_StartTime", "CTOP_End_Time", "AFP_EndTime").Replace(body)
			}
			rows, err := parseFAANAS([]byte(body))
			if err != nil || len(rows) != 1 {
				t.Fatalf("FAA advisories = %+v, error = %v", rows, err)
			}
			got := rows[0]
			if got.StartsAt != nil || got.EndsAt != nil || got.RawStartTime != tc.wantStart || got.RawEndTime != tc.wantEnd {
				t.Fatalf("unexpected clock metadata or fabricated date: %+v", got)
			}
			if tc.wantStart == "" && tc.wantEnd == "" {
				body, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(body), `"rawStartTime"`) || strings.Contains(string(body), `"rawEndTime"`) {
					t.Fatalf("absent clock metadata was serialized: %s", body)
				}
			}
		})
	}
}
