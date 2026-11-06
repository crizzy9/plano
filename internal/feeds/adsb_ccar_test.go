package feeds

import (
	"fmt"
	"testing"
	"time"
)

func TestReadsbKeepsExplicitCanadianRegistrationMarksDistinct(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	body := func(marks string) []byte {
		return []byte(fmt.Sprintf(`{"now":%d,"aircraft":[%s]}`, now.Unix(), marks))
	}
	legacy := `{"hex":"c00003","type":"adsb_icao","r":"CF-ABC","lat":45,"lon":-73,"seen_pos":2}`
	modern := `{"hex":"c00003","type":"adsb_icao","r":"C-FABC","lat":45,"lon":-73,"seen_pos":1}`
	snapshot, err := parseReadsb(body(legacy), now)
	if err != nil {
		t.Fatal(err)
	}
	flight := Flight{Status: "airborne", TailNumber: "C-FABC", ActualTakeoff: new(now.Add(-time.Minute))}
	for _, modeS := range []string{"", "c00003"} {
		if _, _, ok := snapshot.Match(flight, modeS); ok {
			t.Fatalf("a modern mark matched a receiver's explicit legacy mark with address %q", modeS)
		}
	}
	flight.TailNumber = "CF-ABC"
	if _, address, ok := snapshot.Match(flight, "c00003"); !ok || address != "c00003" {
		t.Fatal("explicit legacy mark failed to match")
	}
	snapshot, err = parseReadsb(body(legacy+","+modern), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := snapshot.Match(flight, "c00003"); ok {
		t.Fatal("contradictory receiver marks sharing an address should be ambiguous")
	}
}
