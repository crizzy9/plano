package feeds

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// NOTAMSummary describes the notice text. Its scope never establishes that a
// particular flight is affected or that the notice is currently in force.
type NOTAMSummary struct {
	Scope              string
	Text               string
	Airport            string
	FullAirportClosure bool
	StartsAt           *time.Time
	EndsAt             *time.Time
	HasValidity        bool // A date range or structured ICAO notice was present.
	FixedValidity      bool // Both bounds are unambiguous and the end is definitive.
	dailyClosure       dailyNOTAMWindow
}

type dailyNOTAMWindow struct{ start, duration time.Duration }

const notamCancellationText = "This NOTAM cancels an earlier notice."

// MatchesValidity reports whether a definitive published window agrees with
// every supplied structured bound. Either structured bound may be absent.
func (s NOTAMSummary) MatchesValidity(start, end *time.Time) bool {
	return s.FixedValidity && (start == nil || start.Equal(*s.StartsAt)) && (end == nil || end.Equal(*s.EndsAt))
}

// HasDailyClosure reports whether the notice closes the entire airport on a
// single daily UTC schedule within definitive published validity bounds.
func (s NOTAMSummary) HasDailyClosure() bool { return s.dailyClosure.duration > 0 }

// NextDailyClosure finds the first occurrence that ends strictly after at.
// It can contain at or start later, and is clipped to the published validity.
func (s NOTAMSummary) NextDailyClosure(at time.Time) (start, end time.Time, ok bool) {
	if !s.HasDailyClosure() || !at.Before(*s.EndsAt) {
		return time.Time{}, time.Time{}, false
	}
	if at.Before(*s.StartsAt) {
		at = *s.StartsAt
	}
	const day = 24 * time.Hour
	midnight := at.UTC().Truncate(day)
	// The previous day's closure can run past midnight into this day.
	for _, offset := range [...]time.Duration{-day, 0, day} {
		start = midnight.Add(offset + s.dailyClosure.start)
		end = start.Add(s.dailyClosure.duration)
		if start.Before(*s.StartsAt) {
			start = *s.StartsAt
		}
		if end.After(*s.EndsAt) {
			end = *s.EndsAt
		}
		if end.After(start) && end.After(at) {
			return start, end, true
		}
	}
	return time.Time{}, time.Time{}, false
}

var (
	// FAA Order 7930.2U sections 3-3, 4-2, 5-1, and 8-1 document the
	// domestic contractions, movement area qualifiers, and ICAO fields.
	notamDomesticHeader = regexp.MustCompile(`^![A-Za-z0-9]{2,7} (?:[0-9]{1,2}/[0-9]{3,4} )?([A-Za-z0-9]{2,7}) (.+)$`)
	notamICAOHeader     = regexp.MustCompile(`^\(?([A-Z][0-9]{4}/[0-9]{2})\s*NOTAM([NRC])((?s:.*))$`)
	notamICAOReference  = regexp.MustCompile(`^\s+([A-Z][0-9]{4}/[0-9]{2})$`)
	notamICAOLocation   = regexp.MustCompile(`^[A-Z]{4}$`)
	notamField          = regexp.MustCompile(`(?:^|[\t\r\n ])([QABCDEFG])\)`)
	notamToken          = regexp.MustCompile(`[A-Za-z0-9]+(?:/[A-Za-z0-9]+)?`)
	notamUnit           = regexp.MustCompile(`^([0-9]+)(FT|HR|MIN)$`)
	notamFullClosure    = regexp.MustCompile(`^(?:CLSD|CLOSED)(?: [0-9]{10} ?[-‐‑–] ?(?:[0-9]{10}(?: ?EST)?|PERM|UFN))?[.]?$`)
	notamDomesticRange  = regexp.MustCompile(`(?i)(?:^| )([0-9]{8,12}) ?[-‐‑–] ?([0-9]{0,12}(?: ?[A-Z]{1,4})?)[.]?$`)
	notamDailySchedule  = regexp.MustCompile(`(?i)^DLY ([0-9]{4}) ?[-‐‑–] ?([0-9]{4})$`)
	notamDomesticDaily  = regexp.MustCompile(`(?i)^(?:CLSD|CLOSED) DLY ([0-9]{4}) ?[-‐‑–] ?([0-9]{4}) [0-9]{10} ?[-‐‑–] ?[0-9]{10}[.]?$`)
	notamNonScheduled   = regexp.MustCompile(`(?i)\bNON[- ]SKED\b`)
)

var notamSubjects = []struct{ code, scope, text string }{
	{"AD SEAPLANE BASE", "seaplane_base", "Seaplane base"},
	{"AD HLP", "heliport", "Heliport or helipad"},
	{"AD AP", "airport", "Airport"},
	{"AD", "airport", "Aerodrome"},
	{"RWY", "runway", "Runway"},
	{"TWY", "taxiway", "Taxiway"},
	{"APRON", "apron", "Apron"},
	{"NAV", "navigation", "Navigation aid"},
	{"COM", "communications", "Communications"},
	{"SVC", "service", "Service"},
}

var notamContractions = map[string]string{
	"ACFT": "aircraft", "ADJ": "adjacent to", "AND": "and", "APN": "apron",
	"ARR": "arrival", "AVBL": "available", "BTN": "between", "CLSD": "closed",
	"CTC": "contact", "DEP": "departure", "DLY": "daily", "DUE": "due",
	"EXC": "except", "FOR": "for", "GA": "general aviation", "LDG": "landing",
	"LGT": "light", "LGTS": "lights", "NOT": "not", "OBSC": "obscured",
	"OPN": "open", "OTS": "out of service", "PPR": "prior permission required",
	"RVR": "runway visual range", "RWY": "runway", "SKED": "scheduled",
	"TAX": "taxiing", "TFC": "traffic", "TKOF": "takeoff", "TO": "to", "TRANSIENT": "visiting",
	"TWR": "tower", "TWY": "taxiway", "U/S": "out of service", "VIS": "visibility",
	"WIP": "work in progress",
}

// DecodeNOTAM expands a bounded set of FAA and ICAO contractions. It decodes
// the plain-text E field, D-field schedule, and explicit UTC validity bounds
// when available. A Q code alone cannot establish scope or applicability.
func DecodeNOTAM(raw string) (NOTAMSummary, bool) {
	if len(raw) > 8192 || !utf8.ValidString(raw) {
		return NOTAMSummary{}, false
	}
	parts := notamText(raw)
	if parts.action == 'C' && parts.headerValid {
		return NOTAMSummary{Scope: "cancellation", Text: notamCancellationText, Airport: parts.icaoLocation()}, true
	}
	body, airport := advisoryPlainText(parts.body), parts.airport
	domesticHeader := notamDomesticHeader.FindStringSubmatchIndex(body)
	if domesticHeader != nil {
		if !parts.structured {
			airport = strings.ToUpper(body[domesticHeader[2]:domesticHeader[3]])
		}
		body = body[domesticHeader[4]:]
	}
	if !airportCode.MatchString(airport) {
		airport = ""
	}
	for _, subject := range notamSubjects {
		size := len(subject.code)
		if len(body) <= size || body[size] != ' ' || !strings.EqualFold(body[:size], subject.code) {
			continue
		}
		details := body[size+1:]
		text := subject.text + " " + expandNOTAMText(details)
		if parts.schedule != "" {
			text = strings.TrimRight(text, ".") + ". Published schedule " + expandNOTAMText(parts.schedule) + "."
		}
		if len(text) > 4096 {
			return NOTAMSummary{}, false
		}
		summary := NOTAMSummary{
			Scope: subject.scope, Text: text, Airport: airport,
			FullAirportClosure: subject.scope == "airport" && !parts.hasSchedule && notamFullClosure.MatchString(strings.ToUpper(details)),
		}
		validity := notamDomesticRange.FindStringSubmatch(details)
		if parts.structured {
			summary.HasValidity = true
			// ICAO dates belong in B and C. A second window in E can contradict
			// those dates or qualify the apparent closure in the free text.
			if validity == nil && domesticHeader == nil && parts.icaoLocation() != "" {
				summary.setValidity(parts.start, parts.end)
			}
		} else if validity != nil {
			summary.setValidity(validity[1], validity[2])
		}
		if subject.scope == "airport" && summary.FixedValidity {
			if parts.structured && parts.hasSchedule && !parts.hasLimits && notamFullClosure.MatchString(strings.ToUpper(details)) {
				summary.setDailyClosure(notamDailySchedule.FindStringSubmatch(parts.schedule))
			} else if !parts.structured && subject.code == "AD AP" {
				summary.setDailyClosure(notamDomesticDaily.FindStringSubmatch(details))
			}
		}
		return summary, true
	}
	return NOTAMSummary{}, false
}

type notamParts struct {
	body, schedule, airport, start, end     string
	action                                  byte
	structured, headerValid, referenceValid bool
	hasAirport, hasSchedule, invalidOrder   bool
	hasLimits                               bool
}

func (parts notamParts) icaoLocation() string {
	if parts.invalidOrder || !notamICAOLocation.MatchString(parts.airport) {
		return ""
	}
	return parts.airport
}

func notamText(raw string) notamParts {
	fields := notamField.FindAllStringSubmatchIndex(raw, -1)
	if len(fields) == 0 {
		prefix := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), ")"))
		action, headerValid, referenceValid := parseNOTAMHeader(prefix)
		return notamParts{body: raw, action: action, headerValid: headerValid, referenceValid: referenceValid}
	}
	prefix := strings.TrimSpace(raw[:fields[0][0]])
	parts := notamParts{structured: prefix != "" || len(fields) > 1 || raw[fields[0][2]] != 'E'}
	if prefix != "" {
		parts.action, parts.headerValid, parts.referenceValid = parseNOTAMHeader(prefix)
		if !parts.headerValid {
			parts.body = raw
			parts.structured = false
			return parts
		}
	}
	lastField := -1
	for i, match := range fields {
		field := raw[match[2]]
		position := strings.IndexByte("QABCDEFG", field)
		parts.invalidOrder = parts.invalidOrder || position <= lastField
		lastField = position
		end := len(raw)
		if i+1 < len(fields) {
			end = fields[i+1][0]
		}
		value := strings.TrimSpace(raw[match[1]:end])
		if i+1 == len(fields) && strings.HasPrefix(prefix, "(") {
			value = strings.TrimSpace(strings.TrimSuffix(value, ")"))
		}
		switch field {
		case 'A':
			parts.hasAirport = true
			parts.airport = strings.ToUpper(value)
		case 'B':
			parts.start = value
		case 'C':
			parts.end = value
		case 'D':
			parts.schedule = advisoryPlainText(value)
			parts.hasSchedule = true
		case 'E':
			parts.body = value
		case 'F', 'G':
			parts.hasLimits = true
		}
	}
	return parts
}

func parseNOTAMHeader(prefix string) (action byte, headerValid, referenceValid bool) {
	header := notamICAOHeader.FindStringSubmatch(strings.ToUpper(prefix))
	if header == nil {
		return 0, false, false
	}
	reference := notamICAOReference.FindStringSubmatch(header[3])
	return header[2][0], header[3] == "" || reference != nil,
		reference != nil && header[1][0] == reference[1][0] && header[1] != reference[1]
}

func normalizeLinkedNOTAM(row *Advisory) bool {
	parts := notamText(row.RawReason)
	if parts.action != 'C' && parts.action != 'R' {
		return true
	}
	if !parts.referenceValid || parts.hasAirport && parts.icaoLocation() != row.Airport {
		return false
	}
	if parts.action == 'C' {
		row.Kind = "notam_cancellation"
		if strings.TrimSpace(row.Reason) == "" {
			row.Reason = notamCancellationText
		}
	}
	return true
}

func (summary *NOTAMSummary) setValidity(start, end string) {
	summary.HasValidity = true
	end = strings.ToUpper(end)
	endTime, estimated := strings.CutSuffix(end, "EST")
	summary.StartsAt = notamTimestamp(start)
	summary.EndsAt = notamTimestamp(strings.TrimSpace(endTime))
	summary.FixedValidity = !estimated && summary.StartsAt != nil && summary.EndsAt != nil && summary.EndsAt.After(*summary.StartsAt)
}

func (summary *NOTAMSummary) setDailyClosure(hours []string) {
	if hours == nil || hours[1] == hours[2] {
		return
	}
	start, startErr := time.Parse("1504", hours[1])
	end, endErr := time.Parse("1504", hours[2])
	if startErr != nil || endErr != nil {
		return
	}
	startOffset := time.Duration(start.Hour())*time.Hour + time.Duration(start.Minute())*time.Minute
	duration := end.Sub(start)
	if duration < 0 {
		duration += 24 * time.Hour
	}
	summary.dailyClosure = dailyNOTAMWindow{start: startOffset, duration: duration}
	if _, _, ok := summary.NextDailyClosure(*summary.StartsAt); !ok {
		summary.dailyClosure = dailyNOTAMWindow{}
		return
	}
	summary.FullAirportClosure = true
}

func notamTimestamp(value string) *time.Time {
	if len(value) != 10 {
		return nil
	}
	// Explicitly interpret YY in 2000–2099 instead of Go's two-digit-year pivot.
	parsed, err := time.Parse("200601021504", "20"+value)
	if err != nil {
		return nil
	}
	return &parsed
}

func expandNOTAMText(value string) string {
	value = notamNonScheduled.ReplaceAllString(value, "non-scheduled")
	return notamToken.ReplaceAllStringFunc(value, func(token string) string {
		upper := strings.ToUpper(token)
		if word, ok := notamContractions[upper]; ok {
			return word
		}
		if measurement := notamUnit.FindStringSubmatch(upper); measurement != nil {
			unit := map[string]string{"FT": "ft", "HR": "hr", "MIN": "min"}[measurement[2]]
			return measurement[1] + " " + unit
		}
		return token
	})
}

func applyNOTAMSummary(row *Advisory) {
	switch row.Kind {
	case "notam", AdvisoryAirportNotice, "airport_closure", "airport_restriction", "runway_closure", "taxiway_closure", "apron_closure", "airport_operations":
	default:
		return
	}
	summary, ok := DecodeNOTAM(row.RawReason)
	if !ok {
		return
	}
	if summary.MatchesValidity(row.StartsAt, row.EndsAt) {
		row.StartsAt, row.EndsAt = summary.StartsAt, summary.EndsAt
	}
	if row.Kind == "airport_closure" && !summary.FullAirportClosure {
		row.Kind = "airport_restriction"
		row.Reason = summary.Text
	} else if strings.TrimSpace(row.Reason) == "" {
		row.Reason = summary.Text
	}
}
