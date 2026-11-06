package feeds

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxAdvisoryIngressBytes = 64 << 10

	// AdvisoryMaxAge bounds ordinary advisory observations, including withdrawals.
	AdvisoryMaxAge = 6 * time.Hour
	// TFMSAdvisoryMaxAge bounds canonical FAA airport program observations.
	TFMSAdvisoryMaxAge = 48 * time.Hour
)

type advisoryIngressEnvelope struct {
	Version  int       `json:"version"`
	Source   string    `json:"source"`
	Advisory *Advisory `json:"advisory"`
}

var (
	bridgeAdvisoryKind = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$\z`)
	bridgeAdvisoryID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$\z`)
	bridgeAreaID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.:/+-]{0,127}$\z`)
)

// IsTFMSAirportProgram identifies active FAA TFMS airport context with a
// published program window. A revision can qualify after that window ends.
func (a Advisory) IsTFMSAirportProgram() bool {
	if a.Source != FAASWIM || !a.AirportContextOnly || a.Direction != "arrival" ||
		!airportCode.MatchString(a.Airport) || a.SupersedesID != "" || a.DelayMinutes != nil ||
		a.StartsAt == nil || a.StartsAt.IsZero() || a.EndsAt == nil || a.EndsAt.IsZero() || !a.EndsAt.After(*a.StartsAt) {
		return false
	}
	switch a.Kind {
	case "ground_stop":
		return a.ID == "tfms/gs/"+a.Airport
	case "ground_delay":
		return a.ID == "tfms/gdp/"+a.Airport
	default:
		return false
	}
}

// ObservationAllowed checks the original revision timestamp at signed ingress.
func (a Advisory) ObservationAllowed(now time.Time) bool {
	return !a.ObservedAt.IsZero() && !a.ObservedAt.After(now.Add(30*time.Second)) &&
		!a.ObservedAt.Before(now.Add(-a.observationMaxAge()))
}

// Expiry returns the earlier of the published end and the observation age limit.
func (a Advisory) Expiry() time.Time {
	deadline := a.ObservedAt.Add(a.observationMaxAge())
	if a.EndsAt != nil && a.EndsAt.Before(deadline) {
		return *a.EndsAt
	}
	return deadline
}

func (a Advisory) observationMaxAge() time.Duration {
	if a.IsTFMSAirportProgram() {
		return TFMSAdvisoryMaxAge
	}
	return AdvisoryMaxAge
}

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
	var wire advisoryIngressEnvelope
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
	applyNOTAMSummary(&row)
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
	if row.SupersedesID != "" {
		row.SupersedesID = strings.TrimSpace(row.SupersedesID)
		if row.Airport == "" || row.SupersedesID == row.ID || !bridgeAdvisoryID.MatchString(row.ID) ||
			!bridgeAdvisoryID.MatchString(row.SupersedesID) || !normalizeLinkedNOTAM(row) {
			return false
		}
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
