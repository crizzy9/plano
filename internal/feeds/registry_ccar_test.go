package feeds

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	ccarTestAddress      = "110000000001011101001111"
	ccarTestOtherAddress = "110000000010001110101110"
	ccarTestThirdAddress = "110000000000000000000011"
	ccarTestLastAddress  = "110000000101000111100010"
)

func TestCCARRegistryZIPPreservesMarksAndTechnicalFields(t *testing.T) {
	rows := strings.Join([]string{
		ccarTestRow(" ABC", "Legacy 100", "2000/01/01", "", "1987/01/01", "Registered", ccarTestOtherAddress),
		ccarTestRow("FABC", "Modern 100", "2001/01/01", "", "2008/01/01", "Registered", ccarTestAddress),
		ccarTestRow(" XYZ", "Same 200", "2001/01/01", "", "2008/01/01", "Registered", ccarTestThirdAddress),
		ccarTestRow("FXYZ", "Same 200", "2001/01/01", "", "2008/01/01", "Registered", ccarTestThirdAddress),
		ccarTestRow("GDEF", `Model "Q" 300`, "2003/01/01", "", "2001/01/01", "Registered", ccarTestLastAddress),
		"\r\n5 rows selected.\r\n\r\n\x1a",
	}, "")
	archive := ccarTestZIP(t, rows, ccarTestLayout())
	registry, err := LoadCCARRegistryZIP(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		tail, address, model string
		age                  int
	}{
		{" cf-abc ", "c023ae", "Legacy 100", 39},
		{"c-fabc", "c0174f", "Modern 100", 18},
		{"CF-XYZ", "", "Same 200", 18},
		{"C-FXYZ", "", "Same 200", 18},
		{"CGDEF", "c051e2", `Model "Q" 300`, 25},
		{"C-GDEF", "c051e2", `Model "Q" 300`, 25},
	} {
		entry := registry.entryAt(tc.tail, at)
		age := registry.Age(tc.tail, at)
		if age == nil || *age != tc.age || entry.address != tc.address ||
			entry.model != tc.model || entry.source != CCARRegistrySource {
			t.Errorf("wrong technical details for %s", tc.tail)
		}
	}
	for _, packed := range []string{"CFABC", "CFXYZ"} {
		if !registry.Ambiguous(packed) || registry.entryAt(packed, at) != (registryEntry{}) {
			t.Errorf("packed mark %s resolved across an identity collision", packed)
		}
	}
	for _, unknown := range []string{"FABC", "GDEF", "C--GDEF", "N12AA"} {
		if registry.Ambiguous(unknown) || registry.entryAt(unknown, at) != (registryEntry{}) {
			t.Errorf("unsupported mark %s resolved", unknown)
		}
	}
	for key, entry := range registry.byTail {
		for _, field := range []string{key, entry.address, entry.model} {
			if strings.Contains(field, "PRIVATE") {
				t.Fatal("a personal or serial field was retained")
			}
		}
	}
}

func TestCCARRegistryZIPEnforcesRegistrationWindowsAndSafeValues(t *testing.T) {
	row := ccarTestRow("GXYZ", "Model 100", "2026/10/15", "2026/11/06", "2002/01/01", "Registered", ccarTestThirdAddress)
	rows := row + row +
		ccarTestRow("GEXP", "Inactive", "2001/01/01", "", "1987/01/01", "Registration Expired", ccarTestAddress) +
		ccarTestRow("GCAN", "Inactive", "2001/01/01", "", "1987/01/01", "Registration Cancelled", ccarTestAddress) +
		ccarTestRow("GDUP", "Conflicting 1", "2001/01/01", "", "1987/01/01", "Registered", ccarTestAddress) +
		ccarTestRow("GDUP", "Conflicting 2", "2001/01/01", "", "1987/01/01", "Registered", ccarTestOtherAddress) +
		ccarTestRow("GSAF", "Bad \u202e label", "2001/01/01", "", "2008/01/01", "Registered", ccarTestAddress) +
		ccarTestRow("GWIN", "Bad \x96 label", "2001/01/01", "", "2008/01/01", "Registered", ccarTestOtherAddress) +
		ccarTestRow("GNON", "Model 900", "2001/01/01", "", "", "Registered", "") +
		ccarTestRow("GHEX", "Model 900", "2001/01/01", "", "not-a-date", "Registered", "C0174F") +
		ccarTestRow("GINV", "Model 100", "not-a-date", "", "2008/01/01", "Registered", ccarTestAddress) +
		ccarTestRow("GMIS", "Model 100", "", "", "2008/01/01", "Registered", ccarTestAddress)
	archive := ccarTestZIP(t, rows, ccarTestLayout())
	registry, err := LoadCCARRegistryZIP(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC)
	if registry.ModeSHexAt("C-GXYZ", at) != "" ||
		registry.ModeSHexAt("C-GXYZ", at.Add(-time.Nanosecond)) != "c00003" ||
		registry.Age("C-GXYZ", time.Date(2026, time.October, 14, 23, 59, 0, 0, time.UTC)) != nil {
		t.Fatal("registration interval was not applied as a half-open interval")
	}
	for _, inactive := range []string{"C-GEXP", "C-GCAN"} {
		if registry.entryAt(inactive, at) != (registryEntry{}) {
			t.Errorf("inactive registration %s supplied technical fields", inactive)
		}
	}
	for _, conflict := range []string{"C-GDUP", "CGDUP", "C-GINV", "C-GMIS"} {
		if !registry.Ambiguous(conflict) || registry.entryAt(conflict, at) != (registryEntry{}) {
			t.Errorf("unusable registration %s supplied technical fields", conflict)
		}
	}
	for unsafe, address := range map[string]string{"C-GSAF": "c0174f", "C-GWIN": "c023ae"} {
		if entry := registry.entryAt(unsafe, at); entry.model != "" || entry.manufactured != 2008 || entry.address != address {
			t.Errorf("unsafe label changed independent technical fields for %s", unsafe)
		}
	}
	for _, noAddress := range []string{"C-GNON", "C-GHEX"} {
		if entry := registry.entryAt(noAddress, at); entry.address != "" || entry.model != "Model 900" || entry.manufactured != 0 {
			t.Errorf("missing age or address was inferred for %s", noAddress)
		}
	}
}

func TestCCARRegistryZIPRejectsMalformedArchivesWithoutRecordData(t *testing.T) {
	layout := ccarTestLayout()
	row := ccarTestRow("FABC", "Model 100", "", "", "2001/01/01", "Registered", ccarTestAddress)
	for _, tc := range []struct {
		name, rows, layout string
		extra              []faaTestZIPMember
	}{
		{name: "empty table", layout: layout, extra: []faaTestZIPMember{{name: "carscurr.txt"}}},
		{name: "missing aircraft table", layout: layout},
		{name: "missing layout", rows: row},
		{name: "drifted layout", rows: row, layout: strings.ReplaceAll(layout, "MODEL_NAME", "OWNER_NAME")},
		{name: "short row", rows: row + "PRIVATE,PRIVATE\r\n", layout: layout},
		{name: "broken quoting", rows: row + `"PRIVATE`, layout: layout},
		{name: "oversize record", rows: row + `"PRIVATE` + strings.Repeat("x", faaRecordBytes), layout: layout},
		{name: "wrong count", rows: row + "2 rows selected.", layout: layout},
		{name: "rows after footer", rows: row + "1 row selected.\r\n" + row, layout: layout},
		{name: "rows after DOS EOF", rows: row + "\x1a\r\n" + row, layout: layout},
		{name: "duplicate table", rows: row, layout: layout, extra: []faaTestZIPMember{{name: "CARSCURR.TXT", body: row}}},
		{name: "nested table", rows: row, layout: layout, extra: []faaTestZIPMember{{name: "nested/carscurr.txt", body: row}}},
		{name: "oversize table", layout: layout, extra: []faaTestZIPMember{{name: "carscurr.txt", body: "x", rawSize: ccarAircraftBytes + 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := ccarTestZIP(t, tc.rows, tc.layout, tc.extra...)
			if _, err := LoadCCARRegistryZIP(bytes.NewReader(archive), int64(len(archive))); err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("expected a sanitized registry error, got %v", err)
			}
		})
	}
	if _, err := LoadCCARRegistryZIP(bytes.NewReader(nil), ccarArchiveBytes+1); err == nil {
		t.Fatal("oversize registry archive was accepted")
	}
}

func ccarTestZIP(t *testing.T, rows, layout string, extra ...faaTestZIPMember) []byte {
	t.Helper()
	// An unsupported owner-table compression method proves the loader never opens it.
	files := []faaTestZIPMember{{name: "carsownr.txt", body: "PRIVATE OWNER", rawSize: 1}}
	if rows != "" {
		files = append(files, faaTestZIPMember{name: "carscurr.txt", body: rows})
	}
	if layout != "" {
		files = append(files, faaTestZIPMember{name: "carslayout.txt", body: layout})
	}
	return faaTestZIP(t, append(files, extra...)...)
}

func ccarTestLayout() string {
	var layout strings.Builder
	layout.WriteString("\r\n1 CARSCURR.TXT CARSCURR.TXT FILE\r\n")
	for index := range ccarFieldCount {
		column := ccarTechnicalColumns[index]
		if column == "" {
			column = fmt.Sprintf("IGNORED_%d", index)
		}
		fmt.Fprintf(&layout, "%d %-32s %-32s TEXT\r\n", index+2, column, column)
	}
	layout.WriteString("49 CARSOWNR.TXT CARSOWNR.TXT FILE\r\n50 PRIVATE_FIELD PRIVATE_FIELD TEXT\r\n\x1a")
	return layout.String()
}

func ccarTestRow(mark, model, from, until, manufactured, status, address string) string {
	row := make([]string, ccarFieldCount)
	row[ccarMarkColumn], row[ccarTrimmedMarkColumn] = mark, strings.TrimSpace(mark)
	row[3], row[5], row[7] = "PRIVATE COMMON NAME", "PRIVATE SERIAL", "PRIVATE MANUFACTURER"
	row[ccarModelColumn], row[ccarManufactureColumn] = model, manufactured
	row[ccarEffectiveColumn], row[ccarIneffectiveColumn] = from, until
	row[ccarStatusColumn], row[ccarAddressColumn] = status, address
	for index, value := range row {
		row[index] = `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
	}
	return strings.Join(row, ",") + "\r\n"
}
