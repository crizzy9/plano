package feeds

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// The official ACFTREF export uses padded, comma-delimited slots. Quotes in
// those slots are data, including at the start of a model name. Its first three
// slot widths are documented in https://registry.faa.gov/database/ardata.pdf;
// the complete layout is checked before treating any quotes as literal data.
// CSV files outside this recognized padded profile keep encoding/csv's strict
// quoting rules; within the profile even a CSV-looking quoted label is literal.
type faaReferenceCSVReader struct {
	input          *bufio.Reader
	pending        []byte
	err            error
	configured     bool
	paddedHeader   bool
	formatSelected bool
	padded         bool
}

var faaReferencePaddedColumns = [...]string{
	"CODE", "MFR", "MODEL", "TYPE-ACFT", "TYPE-ENG", "AC-CAT",
	"BUILD-CERT-IND", "NO-ENG", "NO-SEATS", "AC-WEIGHT", "SPEED",
	"TC-DATA-SHEET", "TC-DATA-HOLDER",
}

// Zero-based comma positions in the padded ACFTREF export, excluding CRLF.
var faaReferencePaddedDelimiters = [...]int{7, 38, 59, 61, 64, 66, 68, 71, 75, 83, 88, 104, 155}

func newFAAReferenceCSVReader(input io.Reader) *faaReferenceCSVReader {
	return &faaReferenceCSVReader{input: bufio.NewReaderSize(input, faaRecordBytes)}
}

func (r *faaReferenceCSVReader) columns(columns map[string]int, fieldCount int) {
	r.configured = true
	r.paddedHeader = fieldCount == len(faaReferencePaddedColumns)+1 && len(columns) == len(faaReferencePaddedColumns)
	for i, name := range faaReferencePaddedColumns {
		column, exists := columns[name]
		r.paddedHeader = r.paddedHeader && exists && column == i
	}
}

func (r *faaReferenceCSVReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 && r.err == nil {
		line, err := r.input.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(line) >= faaRecordBytes {
			r.err = errors.New("FAA registry reference record exceeds size limit")
			return 0, r.err
		}
		r.pending, r.err = line, err
		if r.configured && len(bytes.TrimRight(line, "\r\n")) != 0 {
			if !r.formatSelected {
				r.padded = r.paddedHeader && faaReferencePaddedRecord(line)
				r.formatSelected = true
			}
			if r.padded {
				if !faaReferencePaddedRecord(line) {
					r.pending = nil
					r.err = errors.New("invalid FAA registry padded reference record")
					return 0, r.err
				}
				r.pending = quoteFAAReferencePaddedRecord(line)
			}
		}
	}
	if len(r.pending) == 0 {
		return 0, r.err
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func faaReferencePaddedRecord(line []byte) bool {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) != 156 || bytes.Count(line, []byte{','}) != len(faaReferencePaddedDelimiters) {
		return false
	}
	for _, position := range faaReferencePaddedDelimiters {
		if line[position] != ',' {
			return false
		}
	}
	return true
}

func quoteFAAReferencePaddedRecord(line []byte) []byte {
	// Validation fixes both the line length and each delimiter before allocation.
	out := make([]byte, 0, 2*len(line)+2*len(faaReferencePaddedDelimiters))
	start := 0
	for _, end := range faaReferencePaddedDelimiters {
		out = append(out, '"')
		for _, value := range line[start:end] {
			out = append(out, value)
			if value == '"' {
				out = append(out, '"')
			}
		}
		out = append(out, '"', ',')
		start = end + 1
	}
	return append(out, line[start:]...)
}
