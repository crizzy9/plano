package feeds

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var faaNNumber = regexp.MustCompile(`^[1-9][0-9]{0,4}[A-HJ-NP-Z]{0,2}$\z`)

type registryEntry struct {
	manufactured int
	address      string
}

// FAARegistry keeps only aircraft identifiers and manufacture years from the
// releasable FAA master file. Aircraft owner and address columns are discarded.
type FAARegistry struct{ byTail map[string]registryEntry }

// LoadFAARegistry reads the unzipped FAA MASTER.txt comma-delimited file.
// It indexes current US registrations and does not infer historical ownership.
func LoadFAARegistry(input io.Reader) (*FAARegistry, error) {
	reader := csv.NewReader(input)
	reader.TrimLeadingSpace = true
	reader.ReuseRecord = true
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("FAA registry header: %w", err)
	}
	columns := make(map[string]int)
	for i, value := range header {
		columns[strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(value, "\ufeff")))] = i
	}
	numberIndex, hasNumber := columns["N-NUMBER"]
	yearIndex, hasYear := columns["YEAR MFR"]
	hexIndex, hasHex := columns["MODE S CODE HEX"]
	if !hasNumber || !hasYear || !hasHex {
		return nil, errors.New("FAA registry header lacks aircraft identity or manufacture columns")
	}
	registry := &FAARegistry{byTail: make(map[string]registryEntry)}
	for line := 2; ; line++ {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// CSV parser errors can wrap raw record fragments. Keep them private.
			return nil, fmt.Errorf("invalid FAA registry record near row %d", line)
		}
		if line > 1_000_001 {
			return nil, errors.New("FAA registry exceeds record limit")
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
		if prior, exists := registry.byTail[tail]; exists && prior != entry {
			registry.byTail[tail] = registryEntry{}
		} else {
			registry.byTail[tail] = entry
		}
	}
	return registry, nil
}

func (r *FAARegistry) Age(tail string, at time.Time) *int {
	if r == nil || at.IsZero() {
		return nil
	}
	year := r.byTail[normalizeRegistration(tail)].manufactured
	if year == 0 || year > at.Year() {
		return nil
	}
	return new(at.Year() - year)
}

func (r *FAARegistry) ModeSHex(tail string) string {
	if r == nil {
		return ""
	}
	return r.byTail[normalizeRegistration(tail)].address
}
