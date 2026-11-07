package feeds

import (
	"archive/zip"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	faaArchiveBytes   = 128 << 20
	faaDirectoryBytes = 512 << 10
	faaMasterBytes    = 640 << 20
	faaReferenceBytes = 32 << 20
	faaRecordBytes    = 32 << 10
	faaMasterRows     = 1_000_000
	faaReferenceRows  = 100_000
)

var (
	faaNNumber   = regexp.MustCompile(`^[1-9][0-9]{0,4}[A-HJ-NP-Z]{0,2}$\z`)
	faaModelCode = regexp.MustCompile(`^[A-Z0-9]{7}$\z`)
)

type registryEntry struct {
	manufactured int
	address      string
	model        string
	validFrom    time.Time
	validUntil   time.Time
	ambiguous    bool
	source       string
}

const CCARRegistrySource = "transport_canada_ccar"

// Registry keeps aircraft identifiers, manufacture years, and model labels.
type Registry struct {
	byTail  map[string]registryEntry
	key     func(string) string
	sources []*Registry
}

// FAARegistry is the legacy type name for the FAA aircraft index.
type FAARegistry = Registry

// CombineAircraftRegistries shares the source indexes without copying their rows.
func CombineAircraftRegistries(registries ...*Registry) *Registry {
	var sources []*Registry
	for _, registry := range registries {
		if registry != nil {
			sources = append(sources, registry)
		}
	}
	switch len(sources) {
	case 0:
		return nil
	case 1:
		return sources[0]
	default:
		return &Registry{sources: sources}
	}
}

// LoadFAARegistry reads the unzipped FAA MASTER.txt comma-delimited file.
// It indexes current US registrations and does not infer historical ownership.
func LoadFAARegistry(input io.Reader) (*FAARegistry, error) {
	return loadFAARegistry(input, nil)
}

// LoadFAARegistryZIP reads the aircraft master and reference tables from the
// official FAA release ZIP without unpacking the other members.
func LoadFAARegistryZIP(input io.ReaderAt, size int64) (*FAARegistry, error) {
	if size <= 0 || size > faaArchiveBytes {
		return nil, errors.New("FAA registry ZIP has an invalid size")
	}
	bounded := &faaDirectoryReader{ReaderAt: input, remaining: faaDirectoryBytes, opening: true}
	archive, err := zip.NewReader(bounded, size)
	if err != nil {
		return nil, errors.New("invalid FAA registry ZIP directory")
	}
	bounded.opening = false
	var master, reference *zip.File
	for _, file := range archive.File {
		name := strings.ToUpper(strings.ReplaceAll(file.Name, `\`, "/"))
		base := path.Base(name)
		var table **zip.File
		var limit uint64
		switch base {
		case "MASTER.TXT", "MASTER.CSV":
			table, limit = &master, faaMasterBytes
		case "ACFTREF.TXT", "ACFTREF.CSV":
			table, limit = &reference, faaReferenceBytes
		default:
			continue
		}
		if name != base || !file.Mode().IsRegular() || *table != nil {
			return nil, errors.New("FAA registry ZIP has an ambiguous aircraft table")
		}
		if file.UncompressedSize64 > limit {
			return nil, errors.New("FAA registry ZIP aircraft table exceeds size limit")
		}
		*table = file
	}
	if master == nil || reference == nil {
		return nil, errors.New("FAA registry ZIP requires MASTER and ACFTREF tables")
	}
	modelsFile, err := reference.Open()
	if err != nil {
		return nil, errors.New("cannot read FAA registry ZIP reference table")
	}
	models, err := loadFAARegistryModels(modelsFile)
	_ = modelsFile.Close()
	if err != nil {
		return nil, err
	}
	masterFile, err := master.Open()
	if err != nil {
		return nil, errors.New("cannot read FAA registry ZIP master table")
	}
	defer masterFile.Close()
	return loadFAARegistry(masterFile, models)
}

// The ZIP reader walks the central directory before we can select two entries.
// Bound that work independently from the sizes of the compressed tables.
type faaDirectoryReader struct {
	io.ReaderAt
	remaining int
	opening   bool
}

func (r *faaDirectoryReader) ReadAt(p []byte, offset int64) (int, error) {
	if r.opening {
		if len(p) > r.remaining {
			return 0, errors.New("FAA registry ZIP directory exceeds size limit")
		}
		r.remaining -= len(p)
	}
	return r.ReaderAt.ReadAt(p, offset)
}

type faaCSV struct {
	reader     *csv.Reader
	limited    *io.LimitedReader
	duplicates map[string]bool
}

func newFAACSV(input io.Reader) faaCSV {
	limited := &io.LimitedReader{R: input}
	reader := csv.NewReader(limited)
	reader.TrimLeadingSpace = true
	reader.ReuseRecord = true
	return faaCSV{reader: reader, limited: limited, duplicates: make(map[string]bool)}
}

func (r faaCSV) read() ([]string, error) {
	// Reset the budget per logical CSV record so an unterminated quoted field
	// cannot cause the parser to buffer the rest of a table.
	r.limited.N = faaRecordBytes
	row, err := r.reader.Read()
	if r.limited.N == 0 {
		return nil, errors.New("FAA registry record exceeds size limit")
	}
	return row, err
}

func (r faaCSV) columns(table string, requireUnique bool) (map[string]int, error) {
	header, err := r.read()
	if err != nil {
		return nil, fmt.Errorf("invalid FAA registry %s header", table)
	}
	columns := make(map[string]int)
	for i, value := range header {
		name := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(value, "\ufeff")))
		if name == "" {
			continue
		}
		if _, exists := columns[name]; exists {
			// Legacy callers can permit duplicates while profile selection still
			// needs to know that a later correct index hid an earlier name.
			r.duplicates[name] = true
			if requireUnique {
				return nil, fmt.Errorf("duplicate FAA registry %s column", table)
			}
		}
		columns[name] = i
	}
	return columns, nil
}

func loadFAARegistry(input io.Reader, models map[string]string) (*FAARegistry, error) {
	master := newFAAMasterCSVReader(input)
	reader := newFAACSV(master)
	columns, err := reader.columns("master", models != nil)
	if err != nil {
		return nil, err
	}
	numberIndex, hasNumber := columns["N-NUMBER"]
	yearIndex, hasYear := columns["YEAR MFR"]
	hexIndex, hasHex := columns["MODE S CODE HEX"]
	modelIndex, hasModel := columns["MFR MDL CODE"]
	statusIndex, hasStatus := columns["STATUS CODE"]
	if !hasNumber || !hasYear || !hasHex || models != nil && !hasModel {
		return nil, errors.New("FAA registry header lacks aircraft identity or manufacture columns")
	}
	master.columns(columns, reader.reader.FieldsPerRecord, reader.duplicates)
	registry := &FAARegistry{byTail: make(map[string]registryEntry)}
	for line := 2; ; line++ {
		row, err := reader.read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// CSV parser errors can wrap raw record fragments. Keep them private.
			return nil, fmt.Errorf("invalid FAA registry record near row %d", line)
		}
		if line > faaMasterRows+1 {
			return nil, errors.New("FAA registry exceeds record limit")
		}
		if models != nil && hasStatus {
			switch strings.TrimSpace(row[statusIndex]) {
			case "V", "T", "M":
			default:
				continue
			}
		}
		number := strings.ToUpper(strings.TrimSpace(row[numberIndex]))
		if len(number) > 5 || !faaNNumber.MatchString(number) {
			continue
		}
		year, _ := strconv.Atoi(strings.TrimSpace(row[yearIndex]))
		if year < 1900 || year > 9999 {
			year = 0
		}
		address := strings.ToLower(strings.TrimSpace(row[hexIndex]))
		if !modeSHexPattern.MatchString(address) {
			address = ""
		}
		tail := "N" + number
		entry := registryEntry{manufactured: year, address: strings.Clone(address)}
		if models != nil {
			entry.model = models[strings.ToUpper(strings.TrimSpace(row[modelIndex]))]
		}
		if prior, exists := registry.byTail[tail]; exists && prior != entry {
			registry.byTail[tail] = registryEntry{}
		} else {
			registry.byTail[tail] = entry
		}
	}
	return registry, nil
}

func loadFAARegistryModels(input io.Reader) (map[string]string, error) {
	reference := newFAAReferenceCSVReader(input)
	reader := newFAACSV(reference)
	columns, err := reader.columns("reference", true)
	if err != nil {
		return nil, err
	}
	codeIndex, hasCode := columns["CODE"]
	makerIndex, hasMaker := columns["MFR"]
	modelIndex, hasModel := columns["MODEL"]
	if !hasCode || !hasMaker || !hasModel {
		return nil, errors.New("FAA registry reference header lacks aircraft model columns")
	}
	reference.columns(columns, reader.reader.FieldsPerRecord)
	models := make(map[string]string)
	for line := 2; ; line++ {
		row, err := reader.read()
		if errors.Is(err, io.EOF) {
			return models, nil
		}
		if err != nil {
			return nil, fmt.Errorf("invalid FAA registry reference record near row %d", line)
		}
		if line > faaReferenceRows+1 {
			return nil, errors.New("FAA registry reference exceeds record limit")
		}
		code := strings.ToUpper(strings.TrimSpace(row[codeIndex]))
		if !faaModelCode.MatchString(code) {
			continue
		}
		maker, model := strings.TrimSpace(row[makerIndex]), strings.TrimSpace(row[modelIndex])
		label := ""
		if maker != "" && model != "" && len(maker)+len(model)+1 <= 96 {
			candidate := maker + " " + model
			if utf8.ValidString(candidate) && !strings.ContainsFunc(candidate, func(r rune) bool {
				return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
			}) {
				label = candidate
			}
		}
		if prior, exists := models[code]; exists && prior != label {
			label = ""
		}
		models[strings.Clone(code)] = label
	}
}

func (r *Registry) lookup(tail string) (registryEntry, bool) {
	if r == nil {
		return registryEntry{}, false
	}
	for _, source := range r.sources {
		if entry, found := source.lookup(tail); found {
			return entry, true
		}
	}
	key := normalizeRegistration
	if r.key != nil {
		key = r.key
	}
	entry, found := r.byTail[key(tail)]
	return entry, found
}

func (r *Registry) entryAt(tail string, at time.Time) registryEntry {
	entry, _ := r.lookup(tail)
	if entry.ambiguous || !entry.validFrom.IsZero() && at.Before(entry.validFrom) ||
		!entry.validUntil.IsZero() && !at.Before(entry.validUntil) {
		return registryEntry{}
	}
	return entry
}

func (r *Registry) Age(tail string, at time.Time) *int {
	return r.entryAt(tail, at).age(at)
}

func (entry registryEntry) age(at time.Time) *int {
	if at.IsZero() {
		return nil
	}
	year := entry.manufactured
	if year == 0 || year > at.Year() {
		return nil
	}
	return new(at.Year() - year)
}

func (r *Registry) ModeSHex(tail string) string {
	return r.ModeSHexAt(tail, time.Now())
}

// ModeSHexAt returns the address for the recorded registration window.
func (r *Registry) ModeSHexAt(tail string, at time.Time) string {
	return r.entryAt(tail, at).address
}

// Ambiguous reports a registration spelling that maps to multiple aircraft.
func (r *Registry) Ambiguous(tail string) bool {
	entry, _ := r.lookup(tail)
	return entry.ambiguous
}

// Model returns the registry's technical model label.
func (r *Registry) Model(tail string) string {
	return r.entryAt(tail, time.Now()).model
}
