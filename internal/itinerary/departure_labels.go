package itinerary

import (
	"regexp"
	"strings"
	"time"
)

var (
	scheduleLabel = regexp.MustCompile(`(?i)\b(dep(?:art(?:ure|s)?)?|arr(?:ival|ive|ives)?)(?:[ \t]+(date|time))?[ \t]*:[ \t]*`)
	zonedStart    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)
	clockField    = regexp.MustCompile(`(?i)^[ \t,]*(?:at[ \t]+)?([0-9]{1,2}:[0-9]{2})(?:[ \t]*(AM|PM))?([ \t]*(?:UTC|GMT|Z))?[ \t,]*$`)
	clockMarker   = regexp.MustCompile(`(?i)\b(?:[0-9]+[ \t]*:|[0-9]{4}-[0-9]{2}-[0-9]{2}T|[0-9]{1,2}[ \t]*(?:AM|PM)\b)`)
)

type departureField struct {
	value, kind string
}

type scheduleLine struct {
	prefix     string
	departures []departureField
	arrival    bool
}

func (line scheduleLine) labeled() bool {
	return len(line.departures) > 0 || line.arrival
}

type labeledRecord struct {
	leg     Leg
	heading string
}

func labeledTextRecords(lines []string, fallbackDate string) map[int]labeledRecord {
	records := make(map[int]labeledRecord)
	var heading string
	for i := 0; i < len(lines); i++ {
		schedule := splitScheduleLine(lines[i])
		var flights []Leg
		if flightPattern.MatchString(schedule.prefix) {
			flights = addLegs(nil, make(map[Leg]bool), schedule.prefix, "", time.Time{}, 2)
		}
		end := i
		if len(flights) == 1 {
			// Associate only adjacent schedule fields with this flight. Bound the
			// lookahead so blank layout lines cannot connect distant records.
			for j := i + 1; j < len(lines) && j-i <= 6; j++ {
				if strings.TrimSpace(lines[j]) == "" {
					continue
				}
				next := splitScheduleLine(lines[j])
				if !next.labeled() || strings.TrimSpace(next.prefix) != "" || schedule.arrival && len(next.departures) > 0 {
					break
				}
				schedule.departures = append(schedule.departures, next.departures...)
				schedule.arrival = schedule.arrival || next.arrival
				end = j
			}
		}
		if !schedule.labeled() {
			if strings.TrimSpace(lines[i]) != "" {
				heading = simpleDateHeading(lines[i])
			}
			continue
		}
		for j := i; j <= end; j++ {
			records[j] = labeledRecord{}
		}
		switch {
		case len(flights) == 1:
			day := fallbackDate
			if heading != "" {
				day = heading
			}
			switch prefixDates := dates(schedule.prefix); len(prefixDates) {
			case 1:
				day = prefixDates[0]
			case 0:
			default:
				day = ""
			}
			if leg, ok := parseLabeledDeparture(schedule.departures, day); ok {
				leg.Number = flights[0].Number
				records[i] = labeledRecord{leg: leg}
			}
		case len(flights) == 0 && !schedule.arrival && strings.TrimSpace(schedule.prefix) == "":
			if leg, ok := parseLabeledDeparture(schedule.departures, ""); ok && leg.LocalDeparture == "" && leg.UTCDeparture.IsZero() {
				records[i] = labeledRecord{heading: leg.Date}
			}
		}
		heading = records[i].heading
		i = end
	}
	return records
}

func simpleDateHeading(line string) string {
	line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ":"))
	if isoPattern.FindString(line) != line && wordDate.FindString(line) != line {
		return ""
	}
	if found := dates(line); len(found) == 1 {
		return found[0]
	}
	return ""
}

func splitScheduleLine(line string) scheduleLine {
	labels := scheduleLabel.FindAllStringSubmatchIndex(line, -1)
	if len(labels) == 0 {
		return scheduleLine{prefix: line}
	}
	result := scheduleLine{prefix: line[:labels[0][0]]}
	for i, label := range labels {
		if !strings.EqualFold(line[label[2]:label[2]+3], "dep") {
			result.arrival = true
			continue
		}
		end := len(line)
		if i+1 < len(labels) {
			end = labels[i+1][0]
		}
		field := departureField{value: strings.Trim(line[label[1]:end], " \t|;")}
		if label[4] >= 0 {
			field.kind = strings.ToLower(line[label[4]:label[5]])
		}
		result.departures = append(result.departures, field)
	}
	return result
}

func parseLabeledDeparture(fields []departureField, fallbackDate string) (Leg, bool) {
	var leg Leg
	var clock, zone string
	for _, entry := range fields {
		field := entry.value
		zoned := zonedStart.MatchString(field)
		if zoned {
			start, err := time.Parse(time.RFC3339Nano, field)
			if err != nil || !leg.UTCDeparture.IsZero() && !leg.UTCDeparture.Equal(start) {
				return Leg{}, false
			}
			leg.UTCDeparture = start.UTC()
			field = field[:len(time.DateOnly)]
		}
		fieldDates := dates(field)
		if len(fieldDates) > 1 || len(fieldDates) == 1 && leg.Date != "" && leg.Date != fieldDates[0] {
			return Leg{}, false
		}
		if len(fieldDates) == 0 && (entry.kind == "date" || isoPattern.MatchString(field) || wordDate.MatchString(field)) {
			return Leg{}, false
		}
		if len(fieldDates) == 1 {
			leg.Date = fieldDates[0]
			date := isoPattern.FindStringIndex(field)
			if date == nil {
				date = wordDate.FindStringIndex(field)
			}
			before, after := field[:date[0]], field[date[1]:]
			field = after
			if clockMarker.MatchString(before) {
				if clockMarker.MatchString(after) {
					return Leg{}, false
				}
				field = before
			}
		}
		fieldClock, fieldZone := departureClock(field)
		if fieldClock != "" {
			if clock != "" && (clock != fieldClock || zone != fieldZone) {
				return Leg{}, false
			}
			clock, zone = fieldClock, fieldZone
		} else if clockMarker.MatchString(field) || entry.kind == "time" && !zoned {
			return Leg{}, false
		}
	}
	if leg.Date == "" {
		leg.Date = fallbackDate
	}
	if leg.Date == "" {
		return Leg{}, false
	}
	if clock != "" && !leg.UTCDeparture.IsZero() {
		return Leg{}, false
	}
	if zone != "" {
		start, err := time.Parse("2006-01-02 15:04", leg.Date+" "+clock)
		if err != nil {
			return Leg{}, false
		}
		leg.UTCDeparture = start
	} else {
		leg.LocalDeparture = clock
	}
	return leg, true
}

func departureClock(text string) (clock, zone string) {
	match := clockField.FindStringSubmatch(text)
	if match == nil {
		return "", ""
	}
	value, layout := match[1], "15:04"
	if match[2] != "" {
		if strings.HasPrefix(value, "0:") || strings.HasPrefix(value, "00:") {
			return "", ""
		}
		value, layout = value+strings.ToUpper(match[2]), "3:04PM"
	} else if len(value) == 4 {
		value = "0" + value
	}
	parsed, err := time.Parse(layout, value)
	if err != nil {
		return "", ""
	}
	if match[3] != "" {
		zone = "UTC"
	}
	return parsed.Format("15:04"), zone
}

func soleUnlabeledDate(lines []string, records map[int]labeledRecord) string {
	var day, unlabeledDay string
	for i, line := range lines {
		if record, labeled := records[i]; labeled {
			structured := record.leg.Date
			if structured == "" {
				structured = record.heading
			}
			if structured != "" {
				if day != "" && day != structured {
					return ""
				}
				day = structured
			}
			continue
		}
		for _, current := range dates(line) {
			if day != "" && day != current {
				return ""
			}
			day, unlabeledDay = current, current
		}
	}
	return unlabeledDay
}
