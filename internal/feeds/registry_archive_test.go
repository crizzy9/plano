package feeds

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

const faaTestMaster = "N-NUMBER,SERIAL NUMBER,MFR MDL CODE,YEAR MFR,STATUS CODE,MODE S CODE HEX,NAME,STREET,\n" +
	"12AA,PRIVATE SERIAL,0012345,2010,V,A00001,PRIVATE OWNER,PRIVATE ADDRESS,\n" +
	"13AA,PRIVATE SERIAL,0012345,2011,W,A00002,PRIVATE OWNER,PRIVATE ADDRESS,\n" +
	"14AA,PRIVATE SERIAL,0012346,2012,T,A00003,PRIVATE OWNER,PRIVATE ADDRESS,\n" +
	"15AA,PRIVATE SERIAL,0012347,2013,M,A00004,PRIVATE OWNER,PRIVATE ADDRESS,\n" +
	"16AA,PRIVATE SERIAL,0012345,2014,V,A00005,PRIVATE OWNER,PRIVATE ADDRESS,\n" +
	"16AA,PRIVATE SERIAL,0012345,2015,V,A00006,PRIVATE OWNER,PRIVATE ADDRESS,\n"

const faaTestReference = "CODE,MFR,MODEL,TYPE-ACFT,\n" +
	"0012345, Example Aviation , Model 100 ,5,\n" +
	"0012346,Example Aviation,Model 200,5,\n" +
	"0012346,Other Aviation,Model 201,5,\n" +
	"0012347,Example Aviation,Model " + "\u202e" + "300,5,\n"

func TestFAARegistryZIPJoinsPublicAircraftFields(t *testing.T) {
	archive := faaTestZIP(t,
		faaTestZIPMember{name: "MASTER.txt", body: faaTestMaster},
		faaTestZIPMember{name: "acftref.csv", body: faaTestReference},
		faaTestZIPMember{name: "DOCUMENT_INDEX.txt", body: "x", rawSize: 1},
	)
	registry, err := LoadFAARegistryZIP(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC)
	if age := registry.Age(" n12aa ", at); age == nil || *age != 16 || registry.ModeSHex("N12AA") != "a00001" ||
		registry.Model("n12aa") != "Example Aviation Model 100" {
		t.Fatal("valid registration did not retain its age, Mode-S address, and joined model")
	}
	for _, tail := range []string{"N13AA", "N16AA", "EIABC"} {
		if registry.Age(tail, at) != nil || registry.ModeSHex(tail) != "" || registry.Model(tail) != "" {
			t.Errorf("ineligible registration %s retained aircraft details", tail)
		}
	}
	for _, tail := range []string{"N14AA", "N15AA"} {
		if registry.Age(tail, at) == nil || registry.ModeSHex(tail) == "" || registry.Model(tail) != "" {
			t.Errorf("registration %s lost its identity or retained an ambiguous or unsafe model", tail)
		}
	}
	legacy, err := LoadFAARegistry(strings.NewReader(faaTestMaster))
	if err != nil || legacy.ModeSHex("N13AA") != "a00002" || legacy.Model("N12AA") != "" {
		t.Fatalf("plain master CSV changed legacy behavior: %v", err)
	}
}

func TestFAARegistryZIPRejectsMalformedAircraftTables(t *testing.T) {
	master := faaTestZIPMember{name: "MASTER.txt", body: faaTestMaster}
	reference := faaTestZIPMember{name: "ACFTREF.txt", body: faaTestReference}
	for _, tc := range []struct {
		name  string
		files []faaTestZIPMember
	}{
		{"missing reference", []faaTestZIPMember{master}},
		{"duplicate master", []faaTestZIPMember{master, reference, {name: "master.csv", body: faaTestMaster}}},
		{"nested reference", []faaTestZIPMember{master, reference, {name: "nested/ACFTREF.txt", body: faaTestReference}}},
		{"backslash path", []faaTestZIPMember{master, {name: `nested\ACFTREF.txt`, body: faaTestReference}}},
		{"missing model join column", []faaTestZIPMember{{name: master.name, body: "N-NUMBER,YEAR MFR,MODE S CODE HEX\n"}, reference}},
		{"duplicate model column", []faaTestZIPMember{master, {name: reference.name, body: "CODE,MFR,MODEL,MODEL\n"}}},
		{"invalid CSV", []faaTestZIPMember{{name: master.name, body: faaTestMaster + `12AB,PRIVATE"SERIAL`}, reference}},
		{"oversize record", []faaTestZIPMember{{name: master.name, body: faaTestMaster + `12AB,"` + strings.Repeat("PRIVATE ", faaRecordBytes/4)}, reference}},
		{"oversize table", []faaTestZIPMember{{name: master.name, body: "x", rawSize: faaMasterBytes + 1}, reference}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := faaTestZIP(t, tc.files...)
			if _, err := LoadFAARegistryZIP(bytes.NewReader(archive), int64(len(archive))); err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("expected a sanitized registry error, got %v", err)
			}
		})
	}
	if _, err := LoadFAARegistryZIP(bytes.NewReader(nil), faaArchiveBytes+1); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("expected oversize archive error, got %v", err)
	}
}

func TestFAARegistryZIPBoundsTheDirectory(t *testing.T) {
	members := []faaTestZIPMember{
		{name: "MASTER.txt", body: faaTestMaster},
		{name: "ACFTREF.txt", body: faaTestReference},
	}
	for i := range 12 {
		name := string(rune('a'+i)) + strings.Repeat("a", 48<<10)
		members = append(members, faaTestZIPMember{name: name, body: "x"})
	}
	archive := faaTestZIP(t, members...)
	if _, err := LoadFAARegistryZIP(bytes.NewReader(archive), int64(len(archive))); err == nil {
		t.Fatal("oversize ZIP directory was accepted")
	}
}

type faaTestZIPMember struct {
	name, body string
	rawSize    uint64
}

func faaTestZIP(t *testing.T, members ...faaTestZIPMember) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for _, member := range members {
		var out io.Writer
		var err error
		if member.rawSize != 0 {
			// Deliberately unsupported compression. Reading this member would fail.
			out, err = archive.CreateRaw(&zip.FileHeader{Name: member.name, Method: 99,
				CompressedSize64: uint64(len(member.body)), UncompressedSize64: member.rawSize})
		} else {
			out, err = archive.Create(member.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write([]byte(member.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
