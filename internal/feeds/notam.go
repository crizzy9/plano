package feeds

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// NOTAMSummary describes the notice text. Its scope never establishes that a
// particular flight is affected or that the notice is currently in force.
type NOTAMSummary struct {
	Scope              string
	Text               string
	FullAirportClosure bool
}

var (
	// FAA Order 7930.2U sections 3-3, 4-2, 5-1, and 8-1 document the
	// domestic contractions, movement area qualifiers, and ICAO fields.
	notamDomesticHeader = regexp.MustCompile(`^![A-Za-z0-9]{2,7} (?:[0-9]{1,2}/[0-9]{3,4} )?[A-Za-z0-9]{2,7} (.+)$`)
	notamICAOHeader     = regexp.MustCompile(`^\(?[A-Z][0-9]{4}/[0-9]{2}\s+NOTAM([NRC])(?:\s+[A-Z][0-9]{4}/[0-9]{2})?$`)
	notamField          = regexp.MustCompile(`(?:^|[\t\r\n ])([QABCDEFG])\)[\t ]*`)
	notamToken          = regexp.MustCompile(`[A-Za-z0-9]+(?:/[A-Za-z0-9]+)?`)
	notamUnit           = regexp.MustCompile(`^([0-9]+)(FT|HR|MIN)$`)
	notamFullClosure    = regexp.MustCompile(`^(?:CLSD|CLOSED)(?: [0-9]{10} ?[-–] ?(?:[0-9]{10}(?: EST)?|PERM))?[.]?$`)
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
// the plain-text E field when available and keeps any D-field schedule. A Q
// code alone cannot establish closure scope or applicability. The caller keeps
// the original notice and uses structured advisory fields for dates and impact.
func DecodeNOTAM(raw string) (NOTAMSummary, bool) {
	if len(raw) > 8192 || !utf8.ValidString(raw) {
		return NOTAMSummary{}, false
	}
	body, schedule, cancelled := notamText(raw)
	if cancelled {
		return NOTAMSummary{Scope: "cancellation", Text: "This NOTAM cancels an earlier notice."}, true
	}
	body = advisoryPlainText(body)
	if header := notamDomesticHeader.FindStringSubmatchIndex(body); header != nil {
		body = body[header[2]:]
	}
	for _, subject := range notamSubjects {
		size := len(subject.code)
		if len(body) <= size || body[size] != ' ' || !strings.EqualFold(body[:size], subject.code) {
			continue
		}
		details := body[size+1:]
		text := subject.text + " " + expandNOTAMText(details)
		if schedule != "" {
			text = strings.TrimRight(text, ".") + ". Published schedule " + expandNOTAMText(schedule) + "."
		}
		if len(text) > 4096 {
			return NOTAMSummary{}, false
		}
		return NOTAMSummary{
			Scope: subject.scope, Text: text,
			FullAirportClosure: subject.scope == "airport" && schedule == "" && notamFullClosure.MatchString(strings.ToUpper(details)),
		}, true
	}
	return NOTAMSummary{}, false
}

func notamText(raw string) (body, schedule string, cancelled bool) {
	fields := notamField.FindAllStringSubmatchIndex(raw, -1)
	if len(fields) == 0 {
		if header := notamICAOHeader.FindStringSubmatch(strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(raw)), ")")); header != nil && header[1] == "C" {
			return "", "", true
		}
		return raw, "", false
	}
	prefix := strings.ToUpper(strings.TrimSpace(raw[:fields[0][0]]))
	if prefix != "" {
		header := notamICAOHeader.FindStringSubmatch(prefix)
		if header == nil {
			return raw, "", false
		}
		if header[1] == "C" {
			return "", "", true
		}
	}
	for i, match := range fields {
		end := len(raw)
		if i+1 < len(fields) {
			end = fields[i+1][0]
		}
		value := strings.TrimSpace(raw[match[1]:end])
		if i+1 == len(fields) && strings.HasPrefix(prefix, "(") {
			value = strings.TrimSpace(strings.TrimSuffix(value, ")"))
		}
		switch raw[match[2]] {
		case 'D':
			schedule = advisoryPlainText(value)
		case 'E':
			body = value
		}
	}
	return body, schedule, false
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
	case "notam", "airport_closure", "airport_restriction", "runway_closure", "taxiway_closure", "apron_closure", "airport_operations":
	default:
		return
	}
	summary, ok := DecodeNOTAM(row.RawReason)
	if !ok {
		return
	}
	if row.Kind == "airport_closure" && !summary.FullAirportClosure {
		row.Kind = "airport_restriction"
		row.Reason = summary.Text
	} else if strings.TrimSpace(row.Reason) == "" {
		row.Reason = summary.Text
	}
}
