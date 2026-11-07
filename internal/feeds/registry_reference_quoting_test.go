package feeds

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// Synthetic values; no reference rows or owner data from the public archive.
const faaReferenceQuotingHeader = "\ufeffCODE,MFR,MODEL,TYPE-ACFT,TYPE-ENG,AC-CAT,BUILD-CERT-IND,NO-ENG,NO-SEATS,AC-WEIGHT,SPEED,TC-DATA-SHEET,TC-DATA-HOLDER,\r\n"
const faaReferenceQuotingMaster = "N-NUMBER,MFR MDL CODE,YEAR MFR,STATUS CODE,MODE S CODE HEX,\r\n" +
	"123ZZ,ZZ00001,2010,V,A00001,\r\n"

func faaReferenceQuotingRow(maker, model string) string {
	return "ZZ00001," + maker + "," + model + ",4,1,1,0,1,2,1,100,,,\r\n"
}

func faaReferencePaddedTestRow(maker, model string) string {
	return fmt.Sprintf("%-7s,%-30s,%-20s,%1s,%2s,%1s,%1s,%2s,%3s,%7s,%4s,%-15s,%-50s,\r\n",
		"ZZ00001", maker, model, "4", "1", "1", "0", "1", "2", "1", "100", "", "")
}

func TestFAARegistryPaddedReferencePreservesLiteralQuotes(t *testing.T) {
	for _, tc := range []struct {
		name, maker, model string
		noFinalNewline     bool
	}{
		{"unquoted model quotes", "SYNTHETIC MAKER", `Demo "Q"`, false},
		{"unquoted maker quotes", `SYNTHETIC "Q" MAKER`, "Demo", false},
		{"doubled literal quotes", "SYNTHETIC MAKER", `Demo ""Q""`, false},
		{"leading quote with padding", "SYNTHETIC MAKER", `"Q"`, false},
		{"leading quote with suffix", "SYNTHETIC MAKER", `"Q"-Series`, false},
		{"literal unclosed quote in complete slot", "SYNTHETIC MAKER", `"Demo`, false},
		{"complete final record without newline", "SYNTHETIC MAKER", `Demo" Q`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reference := faaReferenceQuotingHeader + faaReferencePaddedTestRow(tc.maker, tc.model)
			if tc.noFinalNewline {
				reference = strings.TrimSuffix(reference, "\r\n")
			}
			archive := faaTestZIP(t,
				faaTestZIPMember{name: "MASTER.txt", body: faaReferenceQuotingMaster},
				faaTestZIPMember{name: "ACFTREF.txt", body: reference})
			registry, err := LoadFAARegistryZIP(bytes.NewReader(archive), int64(len(archive)))
			if err != nil {
				t.Fatal(err)
			}
			age := registry.Age("N123ZZ", time.Date(2026, time.November, 7, 0, 0, 0, 0, time.UTC))
			if registry.Model("N123ZZ") != tc.maker+" "+tc.model || registry.ModeSHex("N123ZZ") != "a00001" || age == nil || *age != 16 {
				t.Fatal("padded reference changed the literal technical label or aircraft join")
			}
		})
	}
}

func TestFAARegistryReferenceKeepsStandardCSV(t *testing.T) {
	for _, reference := range []string{
		faaReferenceQuotingHeader + faaReferenceQuotingRow(`"SYNTHETIC, MAKER"`, `"Demo, ""Q"""`),
		"CODE,EXTRA,MODEL,MFR,\r\nZZ00001,\"FIRST,\r\nSECOND \"\"ITEM\"\"\",\"Demo, \"\"Q\"\"\",\"SYNTHETIC, MAKER\",\r\n",
	} {
		archive := faaTestZIP(t,
			faaTestZIPMember{name: "MASTER.txt", body: faaReferenceQuotingMaster},
			faaTestZIPMember{name: "ACFTREF.txt", body: reference})
		registry, err := LoadFAARegistryZIP(bytes.NewReader(archive), int64(len(archive)))
		if err != nil || registry.Model("N123ZZ") != `SYNTHETIC, MAKER Demo, "Q"` {
			t.Fatalf("standard CSV quoting or reordered columns changed: %v", err)
		}
	}
	// The same quoted label has CSV semantics outside the padded profile.
	models, err := loadFAARegistryModels(strings.NewReader(faaReferenceQuotingHeader + faaReferenceQuotingRow("SYNTHETIC MAKER", `"Q"`)))
	if err != nil || models["ZZ00001"] != "SYNTHETIC MAKER Q" {
		t.Fatalf("generic quoted label was not decoded as CSV: %v", err)
	}
}

func TestFAARegistryReferenceKeepsMalformedGenericCSVStrict(t *testing.T) {
	validReference := faaReferenceQuotingHeader + faaReferenceQuotingRow("SYNTHETIC MAKER", "Demo")
	padded := faaReferenceQuotingHeader + faaReferencePaddedTestRow("SYNTHETIC MAKER", `Demo "Q"`)
	for _, tc := range []struct{ name, reference, master string }{
		{"truncated quoted model", faaReferenceQuotingHeader + "ZZ00001,SYNTHETIC MAKER,\"TRUNCATED", faaReferenceQuotingMaster},
		{"malformed quoted model", faaReferenceQuotingHeader + faaReferenceQuotingRow("SYNTHETIC MAKER", `"BAD"x`), faaReferenceQuotingMaster},
		{"bare quote in generic model", faaReferenceQuotingHeader + faaReferenceQuotingRow("SYNTHETIC MAKER", `Demo" Q`), faaReferenceQuotingMaster},
		{"bare quote in generic maker", faaReferenceQuotingHeader + faaReferenceQuotingRow(`SYNTHETIC" MAKER`, "Demo"), faaReferenceQuotingMaster},
		{"bare quote in header", strings.Replace(padded, ",MODEL,", ",MO\"DEL,", 1), faaReferenceQuotingMaster},
		{"duplicate model header", strings.Replace(padded, ",TYPE-ACFT,", ",MODEL,", 1), faaReferenceQuotingMaster},
		{"wrong complete header", strings.Replace(padded, ",SPEED,", ",EXTRA,", 1), faaReferenceQuotingMaster},
		{"extra empty header", strings.Replace(padded, ",\r\n", ",,\r\n", 1), faaReferenceQuotingMaster},
		{"bare quote in master", validReference, strings.Replace(faaReferenceQuotingMaster, "A00001,", "A00001\"BAD,", 1)},
		{"truncated quote in master", validReference, "N-NUMBER,MFR MDL CODE,YEAR MFR,STATUS CODE,MODE S CODE HEX,\r\n123ZZ,\"TRUNCATED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := faaTestZIP(t, faaTestZIPMember{name: "MASTER.txt", body: tc.master}, faaTestZIPMember{name: "ACFTREF.txt", body: tc.reference})
			_, err := LoadFAARegistryZIP(bytes.NewReader(archive), int64(len(archive)))
			if err == nil || strings.Contains(err.Error(), "SYNTHETIC") || strings.Contains(err.Error(), "TRUNCATED") || strings.Contains(err.Error(), "BAD") {
				t.Fatalf("expected sanitized strict CSV rejection, got %v", err)
			}
		})
	}
}

func TestFAARegistryPaddedReferenceRejectsLayoutChanges(t *testing.T) {
	good := faaReferencePaddedTestRow("SYNTHETIC MAKER", `Demo "Q"`)
	for _, bad := range []string{
		good[:len(good)-4],
		strings.Replace(good, "SYNTHETIC MAKER", "SYNTHETIC,MAKER", 1),
		good[:7] + " " + good[8:38] + "," + good[39:],
		good[:7] + good[8:9] + "," + good[9:], // Same length/count, shifted comma.
		strings.TrimSuffix(good, "\r\n") + "x\r\n",
	} {
		if _, err := loadFAARegistryModels(strings.NewReader(faaReferenceQuotingHeader + good + bad)); err == nil {
			t.Fatal("padded format accepted a truncated, shifted or expanded record")
		}
	}
	// A generic first record cannot opt a later malformed CSV row into padding.
	generic := faaReferenceQuotingHeader + faaReferenceQuotingRow("SYNTHETIC MAKER", "Demo") + good
	if _, err := loadFAARegistryModels(strings.NewReader(generic)); err == nil {
		t.Fatal("changed formats within reference table")
	}
}

func TestFAARegistryPaddedReferenceKeepsConflictsAmbiguous(t *testing.T) {
	archive := faaTestZIP(t,
		faaTestZIPMember{name: "MASTER.txt", body: faaReferenceQuotingMaster},
		faaTestZIPMember{name: "ACFTREF.txt", body: faaReferenceQuotingHeader +
			faaReferencePaddedTestRow("SYNTHETIC MAKER", `Demo" One`) + faaReferencePaddedTestRow("SYNTHETIC MAKER", `Demo" Two`)})
	registry, err := LoadFAARegistryZIP(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || registry.Model("N123ZZ") != "" || registry.ModeSHex("N123ZZ") != "a00001" {
		t.Fatalf("conflicting model labels changed ambiguity or technical identity: %v", err)
	}
}

func TestFAARegistryReferencePreservesRecordBounds(t *testing.T) {
	for _, record := range []string{
		"ZZ00001,SYNTHETIC MAKER," + strings.Repeat(`Q"`, faaRecordBytes) + ",4,1,1,0,1,2,1,100,,,\r\n",
		"ZZ00001,SYNTHETIC MAKER,\"" + strings.Repeat("Q\r\n", faaRecordBytes) + "\",4,1,1,0,1,2,1,100,,,\r\n",
	} {
		input := &faaReferenceQuotingCountReader{reader: strings.NewReader(faaReferenceQuotingHeader + record)}
		if _, err := loadFAARegistryModels(input); err == nil {
			t.Fatal("oversized record was accepted")
		}
		if input.bytes > 2*faaRecordBytes {
			t.Fatalf("read beyond record bound and fixed lookahead: %d", input.bytes)
		}
	}
	archive := faaTestZIP(t,
		faaTestZIPMember{name: "MASTER.txt", body: faaReferenceQuotingMaster},
		faaTestZIPMember{name: "ACFTREF.txt", body: "x", rawSize: faaReferenceBytes + 1})
	if _, err := LoadFAARegistryZIP(bytes.NewReader(archive), int64(len(archive))); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatal("oversize reference table was accepted")
	}
	reference := faaReferenceQuotingHeader + strings.Repeat(faaReferencePaddedTestRow("SYNTHETIC MAKER", `Demo "Q"`), faaReferenceRows+1)
	if _, err := loadFAARegistryModels(strings.NewReader(reference)); err == nil || !strings.Contains(err.Error(), "record limit") {
		t.Fatalf("row limit changed: %v", err)
	}
}

type faaReferenceQuotingCountReader struct {
	reader io.Reader
	bytes  int
}

func (r *faaReferenceQuotingCountReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += n
	return n, err
}
