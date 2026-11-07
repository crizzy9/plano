package feeds

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// Independent wire construction from the published MASTER slot widths. Unused
// fields contain only synthetic non-personal markers for leakage regressions.
var faaMasterTestWidths = [...]int{
	5, 30, 7, 5, 4, 1, 50, 33, 33, 18, 2, 10, 1, 3, 2, 8, 8,
	10, 1, 2, 2, 8, 1, 8, 50, 50, 50, 50, 50, 8, 8, 30, 20, 10,
}

const faaMasterTestReference = "CODE,MFR,MODEL,\r\nZZ00001,SYNTHETIC MAKER,Technical Model,\r\n"

func faaMasterTestHeader() string {
	columns := make([]string, 35)
	for i := range 34 {
		columns[i] = fmt.Sprintf("UNUSED%d", i)
	}
	columns[0], columns[2], columns[4], columns[20], columns[33] = "N-NUMBER", "MFR MDL CODE", "YEAR MFR", "STATUS CODE", "MODE S CODE HEX"
	return "\ufeff" + strings.Join(columns, ",") + "\r\n"
}

func faaMasterTestSlots(number, year, status, address string) map[int]string {
	return map[int]string{0: number, 2: "ZZ00001", 4: year, 20: status, 33: address}
}

func faaMasterTestRecord(t *testing.T, slots map[int]string) string {
	t.Helper()
	var record strings.Builder
	for i, width := range faaMasterTestWidths {
		value := slots[i]
		if len(value) > width {
			t.Fatal("synthetic slot exceeds its documented wire width")
		}
		record.WriteString(value)
		record.WriteString(strings.Repeat(" ", width-len(value)))
		record.WriteByte(',')
	}
	record.WriteString("\r\n")
	return record.String()
}

func faaMasterTestZIP(t *testing.T, master string) (*FAARegistry, error) {
	t.Helper()
	archive := faaTestZIP(t,
		faaTestZIPMember{name: "MASTER.txt", body: master},
		faaTestZIPMember{name: "ACFTREF.txt", body: faaMasterTestReference},
	)
	return LoadFAARegistryZIP(bytes.NewReader(archive), int64(len(archive)))
}

func TestFAAMasterPaddedSlotsJoinOnlyTechnicalIdentity(t *testing.T) {
	for _, kit := range []string{`KIT"MARKER`, `"KIT-MARKER`, `"KIT""MARKER"`} {
		t.Run(fmt.Sprintf("kit-shape-%d", len(kit)), func(t *testing.T) {
			slots := faaMasterTestSlots("123ZZ", "2010", "V", "A00001")
			slots[6], slots[7], slots[32] = `UNUSED"MARKER`, `"UNUSED-MARKER`, kit
			master := faaMasterTestHeader() + "\r\n" + faaMasterTestRecord(t, slots)
			plain, err := LoadFAARegistry(strings.NewReader(master))
			if err != nil {
				t.Fatal(err)
			}
			joined, err := faaMasterTestZIP(t, master)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, time.November, 7, 0, 0, 0, 0, time.UTC)
			for _, registry := range []*FAARegistry{plain, joined} {
				age := registry.Age("N123ZZ", at)
				if age == nil || *age != 16 || registry.ModeSHex("N123ZZ") != "a00001" {
					t.Fatal("padded literal quotes changed the technical identity or age")
				}
			}
			if plain.Model("N123ZZ") != "" || joined.Model("N123ZZ") != "SYNTHETIC MAKER Technical Model" {
				t.Fatal("padded MASTER changed plaintext behavior or the reference model join")
			}
		})
	}
}

func TestFAAMasterPaddedProjectionEmptiesEveryUnusedCellBeforeCSV(t *testing.T) {
	slots := faaMasterTestSlots("123ZZ", "2010", "V", "A00001")
	for i := range 34 {
		if _, used := slots[i]; !used {
			slots[i] = strings.Repeat(`"`, faaMasterTestWidths[i])
		}
	}
	master := newFAAMasterCSVReader(strings.NewReader(faaMasterTestHeader() + faaMasterTestRecord(t, slots)))
	reader := newFAACSV(master)
	columns, err := reader.columns("master", true)
	if err != nil {
		t.Fatal(err)
	}
	master.columns(columns, reader.reader.FieldsPerRecord, reader.duplicates)
	projected, err := io.ReadAll(master)
	if err != nil {
		t.Fatal(err)
	}
	// All source quotes occur in unused slots. Only the five technical CSV
	// wrappers may survive projection; none of those source quote bytes may.
	if bytes.Count(projected, []byte{'"'}) != 10 || len(projected) > 128 {
		t.Fatal("unused slot bytes reached the projected CSV record")
	}
	decoded := newFAACSV(bytes.NewReader(projected))
	row, err := decoded.read()
	if err != nil || len(row) != 35 {
		t.Fatalf("technical projection is not a complete 35-cell CSV record: %v", err)
	}
	for i, value := range row {
		if expected, used := map[int]string{0: "123ZZ", 2: "ZZ00001", 4: "2010", 20: "V", 33: "A00001"}[i]; used {
			if strings.TrimSpace(value) != expected {
				t.Fatal("projection changed a technical slot")
			}
		} else if value != "" {
			t.Fatal("unused slot was decoded rather than replaced with an empty CSV cell")
		}
	}
}

func TestFAAMasterPaddedStatusesConflictsAndUnknownValues(t *testing.T) {
	master := faaMasterTestHeader()
	for _, tc := range []struct{ number, year, status, address string }{
		{"123ZA", "2010", "V", "A00001"}, {"123ZB", "2011", "T", "A00002"},
		{"123ZC", "2012", "M", "A00003"}, {"123ZD", "2013", "W", "A00004"},
		{"123ZE", "2014", "V", "A00005"}, {"123ZE", "2015", "V", "A00006"},
		{"123ZF", "2010", "V", "A00007"}, {"123ZF", "2010", "V", "A00007"},
		{"123ZG", "9999", "V", "A00008"}, {"123ZH", `20"0`, "V", `A00"09`},
	} {
		master += faaMasterTestRecord(t, faaMasterTestSlots(tc.number, tc.year, tc.status, tc.address))
	}
	joined, err := faaMasterTestZIP(t, master)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := LoadFAARegistry(strings.NewReader(master))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.November, 7, 0, 0, 0, 0, time.UTC)
	for _, tail := range []string{"N123ZA", "N123ZB", "N123ZC", "N123ZF"} {
		if joined.Model(tail) == "" || joined.ModeSHex(tail) == "" || joined.Age(tail, at) == nil {
			t.Fatal("accepted status or identical duplicate lost technical fields")
		}
	}
	if joined.Model("N123ZD") != "" || joined.ModeSHex("N123ZD") != "" || plain.ModeSHex("N123ZD") != "a00004" {
		t.Fatal("padded profile changed ZIP filtering or plaintext legacy status behavior")
	}
	for _, registry := range []*FAARegistry{joined, plain} {
		if registry.Model("N123ZE") != "" || registry.ModeSHex("N123ZE") != "" || registry.Age("N123ZE", at) != nil {
			t.Fatal("conflicting technical identities were resolved instead of remaining unknown")
		}
		if registry.Age("N123ZG", at) != nil || registry.Age("N123ZH", at) != nil || registry.ModeSHex("N123ZH") != "" {
			t.Fatal("invalid or future technical values were inferred")
		}
	}
}

func TestFAAMasterPaddedSelectionRejectsEveryLaterLayoutMismatch(t *testing.T) {
	slots := faaMasterTestSlots("123ZZ", "2010", "V", "A00001")
	slots[32] = `KIT"MARKER`
	valid := faaMasterTestRecord(t, slots)
	shifted := []byte(valid)
	shifted[35], shifted[36] = shifted[36], shifted[35]
	if len(shifted) != len(valid) || bytes.Count(shifted, []byte{','}) != strings.Count(valid, ",") {
		t.Fatal("shifted-delimiter fixture changed length or comma count")
	}
	extraComma := []byte(valid)
	extraComma[588] = ','
	for _, tc := range []struct{ name, row string }{
		{"truncated", valid[:len(valid)-9] + "\r\n"},
		{"same length and count, shifted comma", string(shifted)},
		{"extra comma inside slot", string(extraComma)},
		{"extra byte", strings.TrimSuffix(valid, "\r\n") + " \r\n"},
		{"generic CSV after selection", strings.Repeat(",", 34) + "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master := faaMasterTestHeader() + valid + tc.row
			if _, err := faaMasterTestZIP(t, master); err == nil || strings.Contains(err.Error(), "KIT") {
				t.Fatalf("expected a sanitized layout rejection without fallback: %v", err)
			}
			if _, err := LoadFAARegistry(strings.NewReader(master)); err == nil {
				t.Fatal("plaintext padded loader fell through or skipped a malformed record")
			}
		})
	}
}

func TestFAAMasterPaddedHeaderRejectsOverwrittenTechnicalDuplicate(t *testing.T) {
	columns := strings.Split(strings.TrimSuffix(faaMasterTestHeader(), "\r\n"), ",")
	// Legacy generic headers keep their existing last-name-wins behavior. The
	// correct later index must not hide an earlier duplicate when selecting slots.
	columns[1] = "MFR MDL CODE"
	slots := faaMasterTestSlots("123ZZ", "2010", "V", "A00001")
	slots[32] = `KIT"MARKER`
	master := strings.Join(columns, ",") + "\r\n" + faaMasterTestRecord(t, slots)
	if _, err := LoadFAARegistry(strings.NewReader(master)); err == nil {
		t.Fatal("overwritten technical duplicate enabled padded interpretation")
	}
	if _, err := faaMasterTestZIP(t, master); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("ZIP duplicate-header rejection changed: %v", err)
	}
	// Disqualifying the padded profile must not add a generic legacy rejection.
	cells := make([]string, 35)
	cells[0], cells[2], cells[4], cells[20], cells[33] = "123ZZ", "ZZ00001", "2010", "V", "A00001"
	master = strings.Join(columns, ",") + "\r\n" + strings.Join(cells, ",") + "\r\n"
	if registry, err := LoadFAARegistry(strings.NewReader(master)); err != nil || registry.ModeSHex("N123ZZ") != "a00001" {
		t.Fatalf("generic legacy duplicate-header behavior changed: %v", err)
	}
}

func TestFAAMasterGenericCSVStaysStrictAndKeepsQuotedFields(t *testing.T) {
	header := "N-NUMBER,MFR MDL CODE,YEAR MFR,STATUS CODE,MODE S CODE HEX,UNUSED,\r\n"
	valid := "123ZZ,ZZ00001,2010,V,A00001,\"SYNTHETIC, \"\"MARKER\"\"\",\r\n"
	if registry, err := faaMasterTestZIP(t, header+valid); err != nil || registry.Model("N123ZZ") != "SYNTHETIC MAKER Technical Model" {
		t.Fatalf("generic quoted comma/escape handling changed: %v", err)
	}
	for _, invalid := range []string{
		"123ZZ,ZZ00001,2010,V,A00001,BARE\"MARKER,\r\n",
		"123ZZ,ZZ00001,2010,V,A00001,\"TRUNCATED",
		"123ZZ,ZZ00001,2010,V,A00001,\"BAD\"x,\r\n",
	} {
		if _, err := faaMasterTestZIP(t, header+invalid); err == nil {
			t.Fatal("generic malformed quoting was accepted")
		}
		if _, err := LoadFAARegistry(strings.NewReader(header + invalid)); err == nil {
			t.Fatal("plaintext generic malformed quoting was accepted")
		}
	}
	// An eligible header does not permit switching from generic CSV to padded
	// literals after the first data record has already selected generic syntax.
	cells := make([]string, 35)
	cells[0], cells[2], cells[4], cells[20], cells[33] = "123ZZ", "ZZ00001", "2010", "V", "A00001"
	slots := faaMasterTestSlots("124ZZ", "2011", "V", "A00002")
	slots[32] = `KIT"MARKER`
	mixed := faaMasterTestHeader() + strings.Join(cells, ",") + "\r\n" + faaMasterTestRecord(t, slots)
	if _, err := LoadFAARegistry(strings.NewReader(mixed)); err == nil {
		t.Fatal("generic selection later switched to padded literal interpretation")
	}
}

func TestFAAMasterPaddedProfileRequiresTechnicalHeaderPositions(t *testing.T) {
	slots := faaMasterTestSlots("123ZZ", "2010", "V", "A00001")
	slots[32] = `KIT"MARKER`
	for _, name := range []string{"moved model column", "missing status name", "missing trailing empty column"} {
		t.Run(name, func(t *testing.T) {
			columns := strings.Split(strings.TrimSuffix(faaMasterTestHeader(), "\r\n"), ",")
			switch name {
			case "moved model column":
				columns[2], columns[3] = columns[3], columns[2]
			case "missing status name":
				columns[20] = "UNUSED_STATUS"
			case "missing trailing empty column":
				columns = columns[:34]
			}
			master := strings.Join(columns, ",") + "\r\n" + faaMasterTestRecord(t, slots)
			if _, err := faaMasterTestZIP(t, master); err == nil {
				t.Fatal("unrecognized header enabled padded literal interpretation")
			}
		})
	}
}

func TestFAAMasterPaddedWrapperAndLoaderKeepByteAndRowBounds(t *testing.T) {
	input := &faaMasterCountReader{reader: strings.NewReader(strings.Repeat("X", 2*faaRecordBytes))}
	if _, err := io.Copy(io.Discard, newFAAMasterCSVReader(input)); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("MASTER preprocessing did not bound its own physical-line buffer: %v", err)
	}
	if input.bytes > faaRecordBytes {
		t.Fatal("MASTER preprocessing read beyond its fixed physical-line buffer")
	}
	generic := "N-NUMBER,YEAR MFR,MODE S CODE HEX,UNUSED\n123ZZ,2010,A00001,\"" + strings.Repeat("X\n", faaRecordBytes)
	if _, err := LoadFAARegistry(strings.NewReader(generic)); err == nil {
		t.Fatal("generic multiline quoting bypassed the logical CSV record budget")
	}
	slots := faaMasterTestSlots("123ZZ", "2010", "V", "A00001")
	record := faaMasterTestRecord(t, slots)
	rows := &faaMasterRepeatReader{record: record, remaining: faaMasterRows + 1}
	if _, err := LoadFAARegistry(io.MultiReader(strings.NewReader(faaMasterTestHeader()), rows)); err == nil || !strings.Contains(err.Error(), "record limit") {
		t.Fatalf("padded projection bypassed the MASTER row limit: %v", err)
	}
}

type faaMasterCountReader struct {
	reader io.Reader
	bytes  int
}

func (r *faaMasterCountReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += n
	return n, err
}

type faaMasterRepeatReader struct {
	record    string
	remaining int
	offset    int
}

func (r *faaMasterRepeatReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	total := 0
	for total < len(p) && r.remaining > 0 {
		n := copy(p[total:], r.record[r.offset:])
		total, r.offset = total+n, r.offset+n
		if r.offset == len(r.record) {
			r.offset = 0
			r.remaining--
		}
	}
	return total, nil
}
