package feeds

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	CASARegistrySource = "australia_casa"
	casaRegistryBytes  = 32 << 20
	casaRegistryRows   = 100_000
)

// LoadCASARegistry reads the Australian Civil Aircraft Register's acrftreg.csv.
// Only the mark, technical facts, and registration validity enter the index.
func LoadCASARegistry(input io.Reader) (*Registry, error) {
	bounded := &io.LimitedReader{R: input, N: casaRegistryBytes + 1}
	buffered := bufio.NewReader(bounded)
	if prefix, err := buffered.Peek(3); err == nil && string(prefix) == "\ufeff" {
		_, _ = buffered.Discard(3)
	}
	reader := newFAACSV(buffered)
	header, err := reader.read()
	if err != nil {
		return nil, errors.New("invalid CASA registry header")
	}
	columns := make(map[string]int)
	for index, value := range header {
		name := strings.ToLower(strings.TrimSpace(value))
		if name == "" {
			continue
		}
		if _, exists := columns[name]; exists {
			return nil, errors.New("duplicate CASA registry column")
		}
		columns[name] = index
	}
	mark, hasMark := columns["mark"]
	model, hasModel := columns["model"]
	year, hasYear := columns["yearmanu"]
	first, hasFirst := columns["datefirstreg"]
	if !hasMark || !hasModel || !hasYear || !hasFirst {
		return nil, errors.New("CASA registry header lacks required aircraft columns")
	}
	expiry, hasExpiry := columns["regexpirydate"]
	suspension, hasSuspension := columns["suspendstatus"]
	registry := &Registry{byTail: make(map[string]registryEntry), key: casaLookupKey}
	for line := 2; ; line++ {
		offset := reader.reader.InputOffset()
		row, err := reader.read()
		if bounded.N == 0 {
			return nil, errors.New("CASA registry exceeds size limit")
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || reader.reader.InputOffset()-offset > faaRecordBytes {
			return nil, fmt.Errorf("invalid CASA registry record near row %d", line)
		}
		if line > casaRegistryRows+1 {
			return nil, errors.New("CASA registry exceeds record limit")
		}
		tail := casaRegistryKey(row[mark], true)
		if tail == "" {
			continue
		}
		var expiryDate, suspensionStatus string
		if hasExpiry {
			expiryDate = row[expiry]
		}
		if hasSuspension {
			suspensionStatus = row[suspension]
		}
		entry := casaTechnicalEntry(row[model], row[year], row[first], expiryDate, suspensionStatus)
		if prior, exists := registry.byTail[tail]; exists && prior != entry {
			entry = registryEntry{ambiguous: true, source: CASARegistrySource}
		}
		registry.byTail[tail] = entry
	}
	if len(registry.byTail) == 0 {
		return nil, errors.New("CASA registry has no aircraft records")
	}
	return registry, nil
}

func casaLookupKey(tail string) string {
	return casaRegistryKey(tail, false)
}

func casaRegistryKey(value string, allowBare bool) string {
	value = strings.TrimSpace(value)
	for _, ch := range value {
		if (ch < 'A' || ch > 'Z') && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
			return ""
		}
	}
	mark := strings.ToUpper(value)
	if len(mark) != 3 || !allowBare {
		suffix, found := strings.CutPrefix(mark, "VH")
		if !found {
			return ""
		}
		mark = strings.TrimPrefix(suffix, "-")
	}
	if len(mark) != 3 || strings.Contains(mark, "-") {
		return ""
	}
	return "VH-" + mark
}

func casaTechnicalEntry(model, year, first, expiry, suspension string) registryEntry {
	from, validFrom := casaDate(first)
	until, validUntil := casaDate(expiry)
	if strings.TrimSpace(suspension) != "" || !validFrom || !validUntil ||
		!from.IsZero() && !until.IsZero() && !from.Before(until) {
		return registryEntry{ambiguous: true, source: CASARegistrySource}
	}
	entry := registryEntry{validFrom: from, validUntil: until, source: CASARegistrySource}
	year = strings.TrimSpace(year)
	if manufactured, err := strconv.ParseUint(year, 10, 16); len(year) == 4 && err == nil && manufactured >= 1900 {
		entry.manufactured = int(manufactured)
	}
	model = strings.TrimSpace(model)
	if len(model) <= 96 && utf8.ValidString(model) && !strings.ContainsFunc(model, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) {
		entry.model = strings.Clone(model)
	}
	return entry
}

func casaDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, true
	}
	date, err := time.Parse("2 January 2006", value)
	return date, err == nil && date.Year() >= 1900
}
