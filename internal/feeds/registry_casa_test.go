package feeds

import (
	"strings"
	"testing"
	"time"
)

func TestCASARegistryIndexesOnlyCurrentTechnicalFields(t *testing.T) {
	input := "\ufeff\"Mark\",Yearmanu,Model,Serial,regholdName,Datefirstreg,Regexpirydate,Suspendstatus\r\n" +
		"R5B,2009,A321-231,fixture-serial,fixture-holder,06 July 2010,08 May 2028,\r\n" +
		"VH-R5B,2009,A321-231,other-serial,other-holder,06 July 2010,08 May 2028,\r\n" +
		"B02,,737-838,,,07 July 2015,,\r\n" +
		"VH-AGE,2018,,,,,,\r\n"
	registry, err := LoadCASARegistry(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC)
	from := time.Date(2010, time.July, 6, 0, 0, 0, 0, time.UTC)
	until := time.Date(2028, time.May, 8, 0, 0, 0, 0, time.UTC)
	want := registryEntry{
		manufactured: 2009, model: "A321-231", source: CASARegistrySource,
		validFrom: from, validUntil: until,
	}
	for _, mark := range []string{"VH-R5B", " vhr5b ", "vh-r5b"} {
		if got := registry.entryAt(mark, at); got != want {
			t.Fatalf("unexpected technical entry for %q: %+v", mark, got)
		}
		if age := registry.Age(mark, at); age == nil || *age != 17 || registry.ModeSHexAt(mark, at) != "" {
			t.Fatalf("unexpected age or inferred address for %q", mark)
		}
	}
	for _, mark := range []string{"R5B", "N12AA", "C-R5B", "VH--R5B", "VH-R5B0", "VH R5B"} {
		if got := registry.entryAt(mark, at); got != (registryEntry{}) {
			t.Fatalf("matched an invalid or foreign mark %q", mark)
		}
	}
	if registry.entryAt("VH-R5B", from.Add(-time.Second)) != (registryEntry{}) ||
		registry.entryAt("VH-R5B", until) != (registryEntry{}) ||
		registry.entryAt("VH-R5B", from) != want {
		t.Fatal("registration validity window was not enforced")
	}
	if entry := registry.entryAt("VH-B02", at); entry.model != "737-838" || entry.age(at) != nil {
		t.Fatalf("model without manufacture year was lost: %+v", entry)
	}
	if entry := registry.entryAt("VH-AGE", at); entry.model != "" || entry.manufactured != 2018 {
		t.Fatalf("manufacture year without model was lost: %+v", entry)
	}
}

func TestCASARegistryRejectsAmbiguousAndInvalidEntries(t *testing.T) {
	input := "Mark,Model,Yearmanu,Datefirstreg,Regexpirydate,Suspendstatus\n" +
		"DUP,A320,2010,06 July 2010,,\n" +
		"VH-DUP,A321,2010,06 July 2010,,\n" +
		"VHDUP,A320,2010,06 July 2010,,\n" +
		"SUS,A320,2010,06 July 2010,,Suspended\n" +
		"BAD,A320,2010,invalid,,\n" +
		"END,A320,2010,06 July 2010,01 July 2010,\n" +
		"TXT,unsafe\u202emodel,2010,06 July 2010,,\n" +
		"OLD,Model 200,0000,06 July 2010,,\n"
	registry, err := LoadCASARegistry(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC)
	for _, mark := range []string{"VH-DUP", "VH-SUS", "VH-BAD", "VH-END"} {
		if got := registry.entryAt(mark, at); got != (registryEntry{}) {
			t.Fatalf("unsafe registration facts retained for %q", mark)
		}
	}
	if !registry.Ambiguous("VHDUP") {
		t.Fatal("a repeated record restored a conflicting mark")
	}
	if entry := registry.entryAt("VH-TXT", at); entry.model != "" || entry.manufactured != 2010 {
		t.Fatalf("unsafe model should not suppress valid manufacture year: %+v", entry)
	}
	if entry := registry.entryAt("VH-OLD", at); entry.model != "Model 200" || entry.age(at) != nil {
		t.Fatalf("invalid year should not suppress valid model: %+v", entry)
	}
}

func TestCASARegistryBoundsAndSanitizesInvalidCSV(t *testing.T) {
	const header = "Mark,Model,Yearmanu,Datefirstreg,regholdName\n"
	for _, input := range []string{
		"Mark,Model,Yearmanu\n",
		"Mark,Model,Yearmanu,Datefirstreg,yearmanu\n",
		header + "ABC,A321,2010,06 July 2010,\"PRIVATE-ROW",
		header + "ABC,A321,2010,06 July 2010,PRIVATE-ROW" + strings.Repeat("x", faaRecordBytes),
		header,
	} {
		if _, err := LoadCASARegistry(strings.NewReader(input)); err == nil || strings.Contains(err.Error(), "PRIVATE-ROW") {
			t.Fatal("invalid CSV was accepted or its diagnostic exposed row data")
		}
	}
}
