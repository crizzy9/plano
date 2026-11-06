package feeds

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

func TestDecodeCAT062WireCompoundAndBlockBoundaries(t *testing.T) {
	// All supported FRNs in table 1 are present, in edition 1.21 order.
	aircraft := bytes.Join([][]byte{
		{0xff, 0xff, 0xff, 0xfe},             // primary subfield
		{0xab, 0xcd, 0xef}, make([]byte, 16), // address and subfields 2–7
		{0, 1}, make([]byte, 15), // trajectory status and one intent point
		{0x12, 0x34, 0x56, 0x78}, make([]byte, 11), // COM, ADS-B status and subfields 12–14
		make([]byte, 18),                       // subfields 15–21
		make([]byte, 9), {2}, make([]byte, 16), // subfields 22–24 and two BDS reports
		{0, 0, 0, 0, 0xaa, 0xbb}, // subfields 26–28
	}, nil)
	items := [36][]byte{
		1: {0x11, 0x22}, 3: {0x33}, 4: {1, 2, 3}, 5: make([]byte, 8), 6: make([]byte, 6), 7: make([]byte, 4),
		8: {0, 0}, 9: {0, 0}, 10: make([]byte, 7),
		11: aircraft,
		12: {0, 0x42}, 13: {1, 1, 1, 1, 1, 1, 0}, 14: {0xff, 0xe0, 1, 2, 3, 4, 0x56, 0x78, 6, 7, 8, 9, 10},
		15: {0}, 16: append([]byte{0xff, 0xff, 0xff, 0xff, 0xe0}, make([]byte, 31)...),
		17: {0, 0}, 18: {0, 0}, 19: {0, 0}, 20: {0, 0},
		21: append(append(append([]byte{0xff, 0xff, 0xf0}, make([]byte, 34)...), 1, 0, 0, 0, 0), make([]byte, 30)...),
		22: {1, 1, 0}, 23: {0}, 24: append([]byte{0xfe}, make([]byte, 17)...), 25: {0, 0},
		26: {0, 0, 1, 0, 0, 0}, 27: append([]byte{0xff, 0x80}, make([]byte, 17)...),
		28: append([]byte{0xfc}, make([]byte, 13)...), 34: {3, 0x80, 0x0a}, 35: {2, 0x0b},
	}
	full := []byte{0xbf, 0xff, 0xff, 0xff, 0x06}
	for _, item := range items {
		full = append(full, item...)
	}
	minimal := []byte{0x91, 0x0c, 0x10, 0x20, 0x01, 0x02, 0x03, 0x00, 0x43, 0x00}
	stream := append(cat062WireTestBlock(full, minimal), cat062WireTestBlock(minimal)...)
	records, err := decodeCAT062Wire(bytes.NewReader(stream))
	if err != nil || len(records) != 3 {
		t.Fatalf("decode %d records, error %v", len(records), err)
	}
	for frn, want := range items {
		if got := records[0].items[frn]; !bytes.Equal(got, want) {
			t.Errorf("FRN %d = %x, want %x", frn, got, want)
		}
	}
	if aircraft := records[0].aircraft; !bytes.Equal(aircraft[1], []byte{0xab, 0xcd, 0xef}) ||
		!bytes.Equal(aircraft[10], []byte{0x12, 0x34}) || !bytes.Equal(aircraft[11], []byte{0x56, 0x78}) ||
		!bytes.Equal(aircraft[28], []byte{0xaa, 0xbb}) {
		t.Errorf("aircraft fields incorrectly split: %x", aircraft)
	}
	for _, record := range records[1:] {
		if !bytes.Equal(record.items[12], []byte{0, 0x43}) || record.items[34] != nil {
			t.Fatalf("record boundaries incorrect: %x", record.items)
		}
	}
}

func TestDecodeCAT062WireRejectsMalformed(t *testing.T) {
	minimal := []byte{0x91, 0x0c, 0x10, 0x20, 0x01, 0x02, 0x03, 0x00, 0x43, 0x00}
	withRE := []byte{0x91, 0x0d, 1, 1, 4, 0x10, 0x20, 0x01, 0x02, 0x03, 0x00, 0x43, 0x00}
	withAircraft := []byte{0x91, 0x1c, 0x10, 0x20, 0x01, 0x02, 0x03}
	cases := map[string][]byte{
		"empty":                 nil,
		"other category":        {63, 0, 4, 0},
		"partial header":        {62, 0},
		"empty block":           {62, 0, 3},
		"short block":           {62, 0, 2},
		"block exceeds input":   {62, 0, 6, 0, 0},
		"oversized input":       make([]byte, maxCAT062Bytes+1),
		"too many records":      cat062WireTestBlock(bytes.Repeat(minimal, maxCAT062Records+1)),
		"unknown UAP field":     cat062WireTestBlock([]byte{0xd1, 0x0c, 0x10, 0x20, 0x01, 0x02, 0x03, 0x00, 0x43, 0x00}),
		"missing mandatory FRN": cat062WireTestBlock([]byte{0x91, 0x08, 0x10, 0x20, 0x01, 0x02, 0x03, 0x00, 0x43}),
		"extended UAP":          cat062WireTestBlock([]byte{0x91, 0x0d, 1, 1, 1, 0}),
		"truncated item":        cat062WireTestBlock(minimal[:len(minimal)-1]),
		"truncated status FX":   cat062WireTestBlock(minimal[:len(minimal)-1], []byte{1}),
		"unknown status FX":     cat062WireTestBlock(minimal[:len(minimal)-1], []byte{1, 1, 1, 1, 1, 1, 1, 0}),
		"empty reserved field":  cat062WireTestBlock(withRE, []byte{0}),
		"short reserved field":  cat062WireTestBlock(withRE, []byte{3, 0}),
		"unknown compound FX":   cat062WireTestBlock(withAircraft, []byte{1, 1, 1, 1, 0}),
		"unknown TIS FX":        cat062WireTestBlock(withAircraft, []byte{1, 0x80, 1, 0, 0, 0x43, 0}),
		"zero trajectory REP":   cat062WireTestBlock(withAircraft, []byte{1, 0x40, 0, 0, 0x43, 0}),
		"short trajectory REP":  cat062WireTestBlock(withAircraft, []byte{1, 0x40, 1, 0, 0x43, 0}),
		"partial next block":    append(cat062WireTestBlock(minimal), 62, 0),
		"partial next record":   cat062WireTestBlock(minimal, []byte{0x91}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			records, err := decodeCAT062Wire(bytes.NewReader(data))
			if !errors.Is(err, ErrInvalidQuery) || records != nil {
				t.Fatalf("decode %d records, error %v", len(records), err)
			}
		})
	}
	if _, err := decodeCAT062Wire(iotest.ErrReader(io.ErrUnexpectedEOF)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("I/O error = %v", err)
	}
}

func cat062WireTestBlock(records ...[]byte) []byte {
	data := append([]byte{62, 0, 0}, bytes.Join(records, nil)...)
	binary.BigEndian.PutUint16(data[1:3], uint16(len(data)))
	return data
}
