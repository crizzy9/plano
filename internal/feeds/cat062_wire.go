package feeds

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	maxCAT062Bytes   = 256 << 10
	maxCAT062Records = 128
	cat062HeaderSize = 3
	cat062Category   = 62
	cat062REField    = 34
	cat062SPField    = 35
)

type cat062Record struct {
	items    [36][]byte // ASTERIX field reference number (FRN)
	aircraft [29][]byte // I062/380 subfield number
}

var errInvalidCAT062Wire = fmt.Errorf("%w: invalid EUROCONTROL CAT062 1.21 wire format", ErrInvalidQuery)

// Table 1 of EUROCONTROL-SPEC-0149-9 edition 1.21 defines this UAP.
// Variable items are handled in record. Zero entries there are spare FRNs.
var cat062FixedWidths = [36]int{
	1: 2, 3: 1, 4: 3, 5: 8, 6: 6, 7: 4,
	8: 2, 9: 2, 10: 7, 12: 2,
	15: 1, 17: 2, 18: 2, 19: 2, 20: 2,
	23: 1, 25: 2,
}

// Compound subfield widths in sections 5.2.9 and 5.2.21–5.2.26.
// Negative widths indicate a one-octet REP followed by fixed-width entries.
// The trajectory intent status has an FX bit and only one defined octet.
const cat062TrajectoryIntentStatus = -1

var (
	cat062UpdateAgeWidths = [...]int{1, 1, 1, 1, 2, 1, 1, 1, 1, 1}
	cat062TrackAgeWidths  = [...]int{
		1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1,
		1, 1, 1,
	}
	cat062MeasuredWidths = [...]int{2, 4, 2, 2, 2, 1}
	cat062AircraftWidths = [...]int{
		3, 6, 2, 2, 2, 2, 2,
		cat062TrajectoryIntentStatus, -15, 2, 2, 7, 2, 2,
		2, 2, 2, 2, 1, 8, 1,
		6, 2, 1, -8, 2, 2, 2,
	}
	cat062FlightPlanWidths = [...]int{2, 7, 4, 1, 4, 1, 4, 4, 3, 2, 2, -4, 6, 1, 7, 7, 2, 7}
	cat062AccuracyWidths   = [...]int{4, 2, 4, 1, 1, 2, 2, 1}
	cat062Mode5Widths      = [...]int{1, 4, 6, 2, 2, 1, 1}
)

func decodeCAT062Wire(input io.Reader) ([]cat062Record, error) {
	raw, err := io.ReadAll(io.LimitReader(input, maxCAT062Bytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: could not read EUROCONTROL CAT062 track data", ErrUnavailable)
	}
	if len(raw) == 0 || len(raw) > maxCAT062Bytes {
		return nil, errInvalidCAT062Wire
	}
	var records []cat062Record
	for len(raw) > 0 {
		if len(raw) < cat062HeaderSize || raw[0] != cat062Category {
			return nil, errInvalidCAT062Wire
		}
		length := int(binary.BigEndian.Uint16(raw[1:cat062HeaderSize]))
		if length <= cat062HeaderSize || length > len(raw) {
			return nil, errInvalidCAT062Wire
		}
		block := cat062Cursor{remaining: raw[cat062HeaderSize:length]}
		for len(block.remaining) > 0 {
			if len(records) == maxCAT062Records {
				return nil, errInvalidCAT062Wire
			}
			record := block.record()
			if block.invalid {
				return nil, errInvalidCAT062Wire
			}
			records = append(records, record)
		}
		raw = raw[length:]
	}
	return records, nil
}

type cat062Cursor struct {
	remaining []byte
	invalid   bool
}

func (c *cat062Cursor) record() cat062Record {
	var record cat062Record
	present := c.bitmap(len(record.items) - 1)
	// Data source, track time, track number and track status are mandatory.
	const required = 1<<1 | 1<<4 | 1<<12 | 1<<13
	if present&required != required {
		c.invalid = true
		return record
	}
	for field := 1; field < len(record.items) && !c.invalid; field++ {
		if present&(1<<field) == 0 {
			continue
		}
		item := c.remaining
		switch field {
		case 11: // I062/380 Aircraft Derived Data
			c.compound(cat062AircraftWidths[:], record.aircraft[:])
		case 13: // I062/080 Track Status
			c.extended(1, 7)
		case 14: // I062/290 System Track Update Ages
			c.compound(cat062UpdateAgeWidths[:], nil)
		case 16: // I062/295 Track Data Ages
			c.compound(cat062TrackAgeWidths[:], nil)
		case 21: // I062/390 Flight Plan Related Data
			c.compound(cat062FlightPlanWidths[:], nil)
		case 22: // I062/270 Target Size & Orientation
			c.extended(1, 3)
		case 24: // I062/110 Mode 5 Data Reports and Extended Mode 1 Code
			c.compound(cat062Mode5Widths[:], nil)
		case 26: // I062/510 Composed Track Number
			c.extended(3, len(c.remaining)/3)
		case 27: // I062/500 Estimated Accuracies
			c.compound(cat062AccuracyWidths[:], nil)
		case 28: // I062/340 Measured Information
			c.compound(cat062MeasuredWidths[:], nil)
		case cat062REField, cat062SPField:
			c.lengthPrefixed()
		default:
			c.take(cat062FixedWidths[field])
		}
		record.items[field] = item[:len(item)-len(c.remaining)]
	}
	return record
}

func (c *cat062Cursor) take(length int) []byte {
	if c.invalid || length < 1 || length > len(c.remaining) {
		c.invalid = true
		return nil
	}
	field := c.remaining[:length]
	c.remaining = c.remaining[length:]
	return field
}

// FX is the low bit of the final octet of each extent.
func (c *cat062Cursor) extended(width, limit int) []byte {
	for length := width; length <= len(c.remaining) && length <= width*limit; length += width {
		if c.remaining[length-1]&1 == 0 {
			return c.take(length)
		}
	}
	c.invalid = true
	return nil
}

// Bits 8 to 2 of each primary octet are the next seven presence indicators.
func (c *cat062Cursor) bitmap(maxFields int) uint64 {
	primary := c.extended(1, (maxFields+6)/7)
	var fields uint64
	for octet, value := range primary {
		for bit := 0; bit < 7; bit++ {
			if value&(0x80>>bit) != 0 {
				fields |= 1 << (1 + octet*7 + bit)
			}
		}
	}
	if fields>>(maxFields+1) != 0 {
		c.invalid = true
	}
	return fields
}

func (c *cat062Cursor) compound(widths []int, fields [][]byte) {
	present := c.bitmap(len(widths))
	for index, width := range widths {
		if c.invalid {
			return
		}
		field := index + 1
		if present&(1<<field) == 0 {
			continue
		}
		var value []byte
		switch {
		case width == cat062TrajectoryIntentStatus:
			value = c.extended(1, 1)
		case width < 0:
			value = c.repeated(-width)
		default:
			value = c.take(width)
		}
		if fields != nil {
			fields[field] = value
		}
	}
}

func (c *cat062Cursor) repeated(width int) []byte {
	if len(c.remaining) == 0 || c.remaining[0] == 0 {
		c.invalid = true
		return nil
	}
	return c.take(1 + int(c.remaining[0])*width)
}

// Both RE and SP start with an octet count that includes that length octet.
func (c *cat062Cursor) lengthPrefixed() {
	if len(c.remaining) == 0 {
		c.invalid = true
		return
	}
	c.take(int(c.remaining[0]))
}
