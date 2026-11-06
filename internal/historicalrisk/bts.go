package historicalrisk

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	maxRecordBytes = 128 << 10
	maxCSVBytes    = 2 << 30
)

type flightRow struct {
	date, origin, destination string
	bin                       int
	delayed                   bool
}

// csvLimits bounds a complete CSV record, including quoted multiline fields,
// before encoding/csv has to allocate room for it.
type csvLimits struct {
	input                   io.Reader
	totalBytes, recordBytes int64
	quoted                  bool
}

func (r *csvLimits) Read(buffer []byte) (int, error) {
	n, err := r.input.Read(buffer)
	for i, c := range buffer[:n] {
		r.totalBytes++
		r.recordBytes++
		if r.totalBytes > maxCSVBytes {
			return i, errors.New("BTS CSV exceeds 2 GiB")
		}
		if r.recordBytes > maxRecordBytes {
			return i, errors.New("BTS CSV record exceeds 128 KiB")
		}
		switch c {
		case '"':
			r.quoted = !r.quoted
		case '\n':
			if !r.quoted {
				r.recordBytes = 0
			}
		}
	}
	return n, err
}

func readFlights(input io.Reader, use func(flightRow) error) error {
	buffered := bufio.NewReader(input)
	if prefix, err := buffered.Peek(3); err == nil && bytes.Equal(prefix, []byte("\xef\xbb\xbf")) {
		_, _ = buffered.Discard(3)
	}
	reader := csv.NewReader(&csvLimits{input: buffered})
	reader.ReuseRecord = true
	header, err := reader.Read()
	if err != nil {
		return fmt.Errorf("read BTS CSV header: %w", err)
	}
	if len(header) > 256 {
		return errors.New("BTS CSV header has more than 256 columns")
	}
	columns := map[string]int{
		"flightdate": -1, "origin": -1, "dest": -1, "crsdeptime": -1, "depdelay": -1, "cancelled": -1,
	}
	for i, column := range header {
		name := strings.ToLower(strings.TrimSpace(column))
		if previous, required := columns[name]; required {
			if previous != -1 {
				return fmt.Errorf("BTS CSV has duplicate %s columns", name)
			}
			columns[name] = i
		}
	}
	for column, index := range columns {
		if index == -1 {
			return fmt.Errorf("BTS CSV is missing the %s column", column)
		}
	}
	dateColumn, originColumn, destColumn := columns["flightdate"], columns["origin"], columns["dest"]
	timeColumn, delayColumn, cancelColumn := columns["crsdeptime"], columns["depdelay"], columns["cancelled"]
	for line := 2; ; line++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read BTS CSV row %d: %w", line, err)
		}
		if line > maxCSVRows+1 {
			return errors.New("BTS CSV exceeds 3 million flight rows")
		}
		cancelled, cancelErr := strconv.ParseFloat(strings.TrimSpace(record[cancelColumn]), 64)
		delay, delayErr := strconv.ParseFloat(strings.TrimSpace(record[delayColumn]), 64)
		bin, timeOK := departureBin(strings.TrimSpace(record[timeColumn]))
		origin := strings.TrimSpace(record[originColumn])
		destination := strings.TrimSpace(record[destColumn])
		if cancelErr != nil || cancelled != 0 || delayErr != nil || math.IsNaN(delay) || math.IsInf(delay, 0) ||
			!timeOK || !airportCode(origin) || !airportCode(destination) || origin == destination {
			continue
		}
		date := strings.TrimSpace(record[dateColumn])
		if _, err := parseDate(date); err != nil {
			return fmt.Errorf("invalid flight date on BTS CSV row %d: %w", line, err)
		}
		if err := use(flightRow{date: date, origin: origin, destination: destination, bin: bin, delayed: delay >= 15}); err != nil {
			return fmt.Errorf("BTS CSV row %d: %w", line, err)
		}
	}
}

func parseDate(value string) (time.Time, error) {
	date, err := time.Parse(time.DateOnly, value)
	if err != nil || date.Year() < 1987 || date.Year() > 2100 {
		return time.Time{}, errors.New("expected a YYYY-MM-DD date between 1987 and 2100")
	}
	return date, nil
}

func departureBin(value string) (int, bool) {
	if len(value) == 0 || len(value) > 4 {
		return 0, false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	hhmm, _ := strconv.Atoi(value)
	if hhmm == 2400 {
		return 0, true
	}
	if hhmm/100 > 23 || hhmm%100 > 59 {
		return 0, false
	}
	return hhmm / 100 / 3, true
}
