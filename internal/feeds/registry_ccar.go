package feeds

import (
	"archive/zip"
	"bufio"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ccarArchiveBytes  = 16 << 20
	ccarLayoutBytes   = 256 << 10
	ccarAircraftBytes = 64 << 20
	ccarAircraftRows  = 100_000
	ccarFieldCount    = 47

	ccarMarkColumn        = 0
	ccarModelColumn       = 4
	ccarEffectiveColumn   = 22
	ccarIneffectiveColumn = 23
	ccarManufactureColumn = 31
	ccarStatusColumn      = 38
	ccarAddressColumn     = 42
	ccarTrimmedMarkColumn = 46
)

var ccarTechnicalColumns = map[int]string{
	ccarMarkColumn:        "MARK",
	ccarModelColumn:       "MODEL_NAME",
	ccarEffectiveColumn:   "EFFECTIVE_DATE",
	ccarIneffectiveColumn: "INEFFECTIVE_DATE",
	ccarManufactureColumn: "DATE_MANUFACTURE_ASSEMBLY",
	ccarStatusColumn:      "REGISTRATION_AUTH_STATUS_E",
	ccarAddressColumn:     "MODE_S_TRANSPONDER_BINARY",
	ccarTrimmedMarkColumn: "TRIMMED_MARK",
}

// LoadCCARRegistryZIP reads Transport Canada's current aircraft table and its
// layout. The owner table, serial numbers, and manufacturer names are excluded
// from the resulting index.
func LoadCCARRegistryZIP(input io.ReaderAt, size int64) (*Registry, error) {
	if size <= 0 || size > ccarArchiveBytes {
		return nil, errors.New("CCAR registry ZIP has an invalid size")
	}
	bounded := &faaDirectoryReader{ReaderAt: input, remaining: faaDirectoryBytes, opening: true}
	archive, err := zip.NewReader(bounded, size)
	if err != nil {
		return nil, errors.New("invalid CCAR registry ZIP directory")
	}
	bounded.opening = false
	var aircraft, layout *zip.File
	for _, file := range archive.File {
		name := strings.ToUpper(strings.ReplaceAll(file.Name, `\`, "/"))
		base := path.Base(name)
		var table **zip.File
		var limit uint64
		switch base {
		case "CARSCURR.TXT":
			table, limit = &aircraft, ccarAircraftBytes
		case "CARSLAYOUT.TXT":
			table, limit = &layout, ccarLayoutBytes
		default:
			continue
		}
		if name != base || !file.Mode().IsRegular() || *table != nil {
			return nil, errors.New("CCAR registry ZIP has an ambiguous table")
		}
		if file.UncompressedSize64 > limit {
			return nil, errors.New("CCAR registry ZIP table exceeds size limit")
		}
		*table = file
	}
	if aircraft == nil || layout == nil {
		return nil, errors.New("CCAR registry ZIP requires CARSCURR and CARSLAYOUT tables")
	}
	layoutFile, err := layout.Open()
	if err != nil {
		return nil, errors.New("cannot read CCAR registry ZIP layout")
	}
	validLayout := verifyCCARLayout(layoutFile)
	_ = layoutFile.Close()
	if !validLayout {
		return nil, errors.New("unsupported CCAR registry aircraft layout")
	}
	aircraftFile, err := aircraft.Open()
	if err != nil {
		return nil, errors.New("cannot read CCAR registry ZIP aircraft table")
	}
	defer aircraftFile.Close()
	return loadCCARAircraft(aircraftFile)
}

// The layout numbers the table heading as row 1, then the aircraft fields
// from row 2. Check the columns before reading the headerless aircraft CSV.
func verifyCCARLayout(input io.Reader) bool {
	scanner := bufio.NewScanner(input)
	current := false
	columns := 0
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[1] {
		case "CARSCURR.TXT":
			if current || columns != 0 || fields[0] != "1" {
				return false
			}
			current = true
			continue
		case "CARSOWNR.TXT":
			current = false
		}
		if !current {
			continue
		}
		number, err := strconv.Atoi(fields[0])
		if err != nil || number != columns+2 || columns == ccarFieldCount {
			return false
		}
		if expected := ccarTechnicalColumns[columns]; expected != "" &&
			(len(fields) < 3 || fields[1] != expected || fields[2] != expected) {
			return false
		}
		columns++
	}
	return scanner.Err() == nil && columns == ccarFieldCount
}

func loadCCARAircraft(input io.Reader) (*Registry, error) {
	reader := newFAACSV(input)
	reader.reader.FieldsPerRecord = -1
	registry := &Registry{byTail: make(map[string]registryEntry), key: ccarLookupKey}
	rows := 0
	footer, dosEOF := false, false
	for {
		row, err := reader.read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid CCAR registry record near row %d", rows+1)
		}
		if len(row) == 1 && rows > 0 && !dosEOF {
			line := strings.TrimSpace(row[0])
			switch {
			case line == "\x1a":
				dosEOF = true
				continue
			case !footer && ccarRowsSelected(strings.TrimSuffix(line, "\x1a"), rows):
				footer = true
				dosEOF = strings.HasSuffix(line, "\x1a")
				continue
			}
		}
		if len(row) != ccarFieldCount || footer || dosEOF {
			return nil, fmt.Errorf("invalid CCAR registry aircraft layout near row %d", rows+1)
		}
		rows++
		if rows > ccarAircraftRows {
			return nil, errors.New("CCAR registry exceeds record limit")
		}
		if strings.TrimSpace(row[ccarStatusColumn]) != "Registered" {
			continue
		}
		mark := strings.TrimSpace(row[ccarMarkColumn])
		if mark != strings.TrimSpace(row[ccarTrimmedMarkColumn]) {
			continue
		}
		tail := ccarCanonicalMark(mark)
		if tail == "" {
			continue
		}
		entry := ccarTechnicalEntry(row)
		if prior, exists := registry.byTail[tail]; exists && prior != entry {
			entry = registryEntry{ambiguous: true, source: CCARRegistrySource}
		}
		registry.byTail[tail] = entry
	}
	if rows == 0 {
		return nil, errors.New("CCAR registry has no aircraft records")
	}
	dropSharedCCARAddresses(registry.byTail)
	for tail, entry := range registry.byTail {
		if !strings.Contains(tail, "-") {
			continue
		}
		packed := strings.ReplaceAll(tail, "-", "")
		if _, exists := registry.byTail[packed]; exists {
			// The same packed key can stand for CF-ABC and C-FABC, even
			// when their recorded aircraft metadata happens to agree.
			entry = registryEntry{ambiguous: true, source: CCARRegistrySource}
		}
		registry.byTail[packed] = entry
	}
	return registry, nil
}

// An address shared by explicit marks cannot independently identify an
// aircraft on radar. Preserve the other technical fields for both marks.
func dropSharedCCARAddresses(entries map[string]registryEntry) {
	byAddress := make(map[string]string, len(entries))
	for tail, entry := range entries {
		if entry.address == "" {
			continue
		}
		prior, exists := byAddress[entry.address]
		if !exists {
			byAddress[entry.address] = tail
			continue
		}
		first := entries[prior]
		first.address, entry.address = "", ""
		entries[prior], entries[tail] = first, entry
	}
}

func ccarRowsSelected(line string, rows int) bool {
	fields := strings.Fields(line)
	if len(fields) != 3 || (fields[1] != "row" && fields[1] != "rows") || fields[2] != "selected." {
		return false
	}
	count, err := strconv.Atoi(fields[0])
	return err == nil && count == rows
}

func ccarLookupKey(tail string) string {
	return strings.ToUpper(strings.TrimSpace(tail))
}

func ccarCanonicalMark(mark string) string {
	for _, letter := range mark {
		if letter < 'A' || letter > 'Z' {
			return ""
		}
	}
	switch {
	case len(mark) == 3:
		return "CF-" + mark
	case len(mark) == 4 && strings.ContainsRune("FGI", rune(mark[0])):
		return "C-" + mark
	default:
		return ""
	}
}

func ccarTechnicalEntry(row []string) registryEntry {
	from, validFrom := ccarDate(row[ccarEffectiveColumn])
	until, validUntil := ccarDate(row[ccarIneffectiveColumn])
	if !validFrom || from.IsZero() || !validUntil || !until.IsZero() && !from.Before(until) {
		return registryEntry{ambiguous: true, source: CCARRegistrySource}
	}
	entry := registryEntry{validFrom: from, validUntil: until, source: CCARRegistrySource}
	manufactured, validManufacture := ccarDate(row[ccarManufactureColumn])
	if validManufacture && manufactured.Year() >= 1900 {
		entry.manufactured = manufactured.Year()
	}
	binary := strings.TrimSpace(row[ccarAddressColumn])
	if value, err := strconv.ParseUint(binary, 2, 24); len(binary) == 24 && err == nil && value != 0 && value != 0xffffff {
		entry.address = fmt.Sprintf("%06x", value)
	}
	model := strings.TrimSpace(row[ccarModelColumn])
	if len(model) <= 96 && utf8.ValidString(model) && !strings.ContainsFunc(model, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) {
		entry.model = strings.Clone(model)
	}
	return entry
}

func ccarDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, true
	}
	date, err := time.Parse("2006/01/02", value)
	return date, err == nil
}
