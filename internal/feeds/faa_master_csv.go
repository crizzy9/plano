package feeds

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// The documented MASTER export has 34 padded slots and a trailing empty field.
// https://registry.faa.gov/database/ardata.pdf defines the widths; no CSV quote
// escaping policy is specified. Only the complete recognized layout treats slot
// bytes literally. Generic CSV retains the strict reader's normal quoting rules.
type faaMasterCSVReader struct {
	input          *bufio.Reader
	pending        []byte
	err            error
	configured     bool
	paddedHeader   bool
	formatSelected bool
	padded         bool
}

var faaMasterTechnicalColumns = [...]struct {
	name  string
	index int
}{
	{"N-NUMBER", 0}, {"MFR MDL CODE", 2}, {"YEAR MFR", 4},
	{"STATUS CODE", 20}, {"MODE S CODE HEX", 33},
}

// Zero-based comma positions, excluding the physical line terminator.
var faaMasterPaddedDelimiters = [...]int{
	5, 36, 44, 50, 55, 57, 108, 142, 176, 195, 198, 209, 211, 215, 218,
	227, 236, 247, 249, 252, 255, 264, 266, 275, 326, 377, 428, 479, 530,
	539, 548, 579, 600, 611,
}

func newFAAMasterCSVReader(input io.Reader) *faaMasterCSVReader {
	return &faaMasterCSVReader{input: bufio.NewReaderSize(input, faaRecordBytes)}
}

func (r *faaMasterCSVReader) columns(columns map[string]int, fieldCount int, duplicates map[string]bool) {
	r.configured = true
	r.paddedHeader = fieldCount == len(faaMasterPaddedDelimiters)+1
	for _, column := range faaMasterTechnicalColumns {
		index, exists := columns[column.name]
		r.paddedHeader = r.paddedHeader && exists && index == column.index && !duplicates[column.name]
	}
}

func (r *faaMasterCSVReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 && r.err == nil {
		// Return only this physical line: columns() must configure the profile
		// after the strict CSV header finishes and before any data is emitted.
		line, err := r.input.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(line) >= faaRecordBytes {
			r.err = errors.New("FAA registry master record exceeds size limit")
			return 0, r.err
		}
		r.pending, r.err = line, err
		if r.configured && len(bytes.TrimRight(line, "\r\n")) != 0 {
			if !r.formatSelected {
				r.padded = r.paddedHeader && faaMasterPaddedRecord(line)
				r.formatSelected = true
			}
			if r.padded {
				if !faaMasterPaddedRecord(line) {
					r.pending = nil
					r.err = errors.New("invalid FAA registry padded master record")
					return 0, r.err
				}
				r.pending = projectFAAMasterPaddedRecord(line)
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

func faaMasterPaddedRecord(line []byte) bool {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) != 612 || bytes.Count(line, []byte{','}) != len(faaMasterPaddedDelimiters) {
		return false
	}
	for _, position := range faaMasterPaddedDelimiters {
		if line[position] != ',' {
			return false
		}
	}
	return true
}

func projectFAAMasterPaddedRecord(line []byte) []byte {
	// Only the five technical slots reach CSV decoding. Never copy unused
	// owner, address, serial or kit-label bytes into the normalized record.
	// Their complete fixed positions were validated before this allocation.
	out := make([]byte, 0, 128)
	start, technical := 0, 0
	for index, end := range faaMasterPaddedDelimiters {
		if technical < len(faaMasterTechnicalColumns) && index == faaMasterTechnicalColumns[technical].index {
			out = append(out, '"')
			for _, value := range line[start:end] {
				out = append(out, value)
				if value == '"' {
					out = append(out, '"')
				}
			}
			out = append(out, '"')
			technical++
		}
		out = append(out, ',')
		start = end + 1
	}
	return append(out, line[start:]...)
}
