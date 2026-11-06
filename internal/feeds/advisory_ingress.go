package feeds

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxAdvisoryIngressBytes = 64 << 10

var (
	bridgeAdvisoryKind = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$\z`)
	bridgeAdvisoryID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$\z`)
	bridgeAreaID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.:/+-]{0,127}$\z`)
)

// DecodeAdvisoryIngress reads one normalized Plano bridge advisory. The caller
// authenticates the operator bridge separately and decides replay and freshness.
// The envelope is not the native FAA SCDS or EUROCONTROL NM B2B format.
func DecodeAdvisoryIngress(input io.Reader) (Advisory, error) {
	raw, err := io.ReadAll(io.LimitReader(input, MaxAdvisoryIngressBytes+1))
	if err != nil {
		return Advisory{}, fmt.Errorf("%w: could not read advisory ingress", ErrInvalidQuery)
	}
	if len(raw) > MaxAdvisoryIngressBytes {
		return Advisory{}, fmt.Errorf("%w: advisory ingress exceeds %d bytes", ErrInvalidQuery, MaxAdvisoryIngressBytes)
	}
	var wire struct {
		Version  int       `json:"version"`
		Source   string    `json:"source"`
		Advisory *Advisory `json:"advisory"`
	}
	if !utf8.Valid(raw) || json.Unmarshal(raw, &wire) != nil || wire.Version != 1 ||
		wire.Source != FAASWIM && wire.Source != Eurocontrol || wire.Advisory == nil {
		return Advisory{}, fmt.Errorf("%w: invalid advisory ingress envelope", ErrInvalidQuery)
	}
	row := *wire.Advisory
	if row.Source != "" && row.Source != wire.Source || !normalizeBridgeAdvisory(&row, wire.Source) ||
		!bridgeAdvisoryID.MatchString(row.ID) || row.Area != "" && !bridgeAreaID.MatchString(row.Area) ||
		row.StartsAt != nil && row.StartsAt.IsZero() || row.EndsAt != nil && row.EndsAt.IsZero() {
		return Advisory{}, fmt.Errorf("%w: invalid advisory ingress record", ErrInvalidQuery)
	}
	row.ObservedAt = row.ObservedAt.UTC()
	if row.StartsAt != nil {
		row.StartsAt = new(row.StartsAt.UTC())
	}
	if row.EndsAt != nil {
		row.EndsAt = new(row.EndsAt.UTC())
	}
	row.Reason = advisoryPlainText(row.Reason)
	return row, nil
}

func normalizeBridgeAdvisory(row *Advisory, source string) bool {
	row.ID = strings.TrimSpace(row.ID)
	row.Airport = strings.ToUpper(strings.TrimSpace(row.Airport))
	row.Area = strings.TrimSpace(row.Area)
	if row.ID == "" || len(row.ID) > 128 || !bridgeAdvisoryKind.MatchString(row.Kind) || row.ObservedAt.IsZero() ||
		row.Airport == "" && row.Area == "" || row.Airport != "" && !airportCode.MatchString(row.Airport) || len(row.Area) > 128 ||
		len(row.Reason) > 4096 || len(row.RawReason) > 8192 ||
		row.Direction != "" && row.Direction != "arrival" && row.Direction != "departure" ||
		row.DelayMinutes != nil && (*row.DelayMinutes < 0 || *row.DelayMinutes > 1440) ||
		row.StartsAt != nil && row.EndsAt != nil && !row.EndsAt.After(*row.StartsAt) {
		return false
	}
	row.Source = source
	return true
}

func advisoryPlainText(reason string) string {
	// Preserve readable words and comparison signs while eliminating invisible
	// formatting controls, multiline injection, and ASCII markup delimiters.
	reason = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r) || unicode.IsSpace(r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		case r == '<':
			return '＜'
		case r == '>':
			return '＞'
		default:
			return r
		}
	}, reason)
	return strings.Join(strings.Fields(reason), " ")
}
