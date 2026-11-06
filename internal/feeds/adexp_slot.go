package feeds

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const MaxADEXPATFMSlotBytes = 16 << 10

var (
	adexpSlotIFPLID  = regexp.MustCompile(`^[A-Z]{2}[0-9]{8}$`)
	adexpSlotARCID   = regexp.MustCompile(`^[A-Z0-9]{2,7}$`)
	adexpSlotCause   = regexp.MustCompile(`^([CWG])([ADE]) [0-9]{2}$`)
	errADEXPATFMSlot = fmt.Errorf("%w: invalid EUROCONTROL ADEXP slot", ErrInvalidQuery)
)

// ADEXPATFMSlotBinding supplies the independently matched AeroAPI flight and
// original source observation when it is absent from the message.
type ADEXPATFMSlotBinding struct {
	ProviderFlightID   string
	IFPLID             string
	ScheduledDeparture time.Time
	ObservedAt         time.Time
	TakeoffDate        string // UTC YYYY-MM-DD when the message has no CTOD.
}

// DecodeADEXPATFMSlot converts one SAM, SRM or SLC. The verified binding supplies
// the airline schedule, and CTOD or the binding supplies the takeoff date.
func DecodeADEXPATFMSlot(input io.Reader, binding ADEXPATFMSlotBinding, now time.Time) (ATFMSlot, error) {
	body, err := io.ReadAll(io.LimitReader(input, MaxADEXPATFMSlotBytes+1))
	if err != nil {
		return ATFMSlot{}, fmt.Errorf("%w: could not read EUROCONTROL ADEXP slot", ErrUnavailable)
	}
	if len(body) > MaxADEXPATFMSlotBytes {
		return ATFMSlot{}, errADEXPATFMSlot
	}
	fields, ok := adexpSlotFields(body)
	if !ok || !adexpSlotIFPLID.MatchString(binding.IFPLID) || fields["IFPLID"] != binding.IFPLID ||
		!adexpSlotARCID.MatchString(fields["ARCID"]) || fields["ADEP"] == "ZZZZ" || fields["ADEP"] == "AFIL" || fields["ADES"] == "ZZZZ" {
		return ATFMSlot{}, errADEXPATFMSlot
	}
	if _, ok := parseADEXPSlotTime("060102", fields["EOBD"]); !ok {
		return ATFMSlot{}, errADEXPATFMSlot
	}
	if _, ok := parseADEXPSlotTime("1504", fields["EOBT"]); !ok {
		return ATFMSlot{}, errADEXPATFMSlot
	}
	observed := binding.ObservedAt
	if fields["TIMESTAMP"] != "" {
		stamp, ok := parseADEXPSlotTime("060102150405", fields["TIMESTAMP"])
		if !ok || !observed.IsZero() && !observed.Equal(stamp) {
			return ATFMSlot{}, errADEXPATFMSlot
		}
		observed = stamp
	}
	status, takeoff, ok := adexpSlotAssignment(fields, binding.TakeoffDate)
	if !ok {
		return ATFMSlot{}, errADEXPATFMSlot
	}
	slot := ATFMSlot{
		Version: 1, Source: Eurocontrol, ProviderFlightID: binding.ProviderFlightID,
		OriginICAO: fields["ADEP"], DestinationICAO: fields["ADES"], ScheduledDeparture: binding.ScheduledDeparture,
		ObservedAt: observed, Status: status, TakeoffAt: takeoff,
	}
	if status == ATFMAssigned {
		slot.Reason = adexpSlotReason(fields["REGCAUSE"])
	}
	return normalizeATFMSlot(slot, now)
}

func adexpSlotAssignment(fields map[string]string, date string) (string, *time.Time, bool) {
	var clock string
	switch fields["TITLE"] {
	case "SAM":
		if fields["NEWCTOT"] != "" {
			return "", nil, false
		}
		clock = fields["CTOT"]
	case "SRM":
		if fields["CTOT"] != "" {
			return "", nil, false
		}
		clock = fields["NEWCTOT"]
	case "SLC":
		return ATFMWithdrawn, nil, date == "" && fields["CTOD"] == "" && fields["CTOT"] == "" && fields["NEWCTOT"] == ""
	default:
		return "", nil, false
	}
	if fields["CTOD"] != "" {
		day, ok := parseADEXPSlotTime("060102", fields["CTOD"])
		if !ok || date != "" && date != day.Format(time.DateOnly) {
			return "", nil, false
		}
		date = day.Format(time.DateOnly)
	}
	takeoff, ok := parseADEXPSlotTime(time.DateOnly+" 1504", date+" "+clock)
	return ATFMAssigned, &takeoff, ok
}

func parseADEXPSlotTime(layout, value string) (time.Time, bool) {
	parsed, err := time.Parse(layout, value)
	return parsed, err == nil && parsed.Format(layout) == value
}

// ADEXP 4.0 §§5.1.3–5.1.5 reserves hyphens for field starts and allows fields
// on one line. TTO and ADDR subfields are parsed separately so neither can
// supply top-level flight identity.
func adexpSlotFields(body []byte) (map[string]string, bool) {
	for _, b := range body {
		if !(b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune(" \r\n\t()-?:.,'=+/", rune(b))) {
			return nil, false
		}
	}
	parts := strings.Split(string(body), "-")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) != "" {
		return nil, false
	}
	fields := make(map[string]string)
	var hasAddressList bool
	for i := 1; i < len(parts); i++ {
		key, value := splitADEXPSlotField(parts[i])
		if i == 1 && key != "TITLE" {
			return nil, false
		}
		switch key {
		case "TTO":
			if value != "" {
				return nil, false
			}
			size, ok := adexpSlotTTO(parts[i+1:])
			if !ok {
				return nil, false
			}
			i += size
		case "BEGIN":
			if value != "ADDR" || hasAddressList {
				return nil, false
			}
			size, ok := adexpSlotAddressList(parts[i+1:])
			if !ok {
				return nil, false
			}
			i += size
			hasAddressList = true
		case "IOBD":
			if _, ok := parseADEXPSlotTime("060102", value); !ok {
				return nil, false
			}
		case "IOBT":
			if _, ok := parseADEXPSlotTime("1504", value); !ok {
				return nil, false
			}
		case "REGUL", "REGLOC", "TAXITIME", "RVR", "REASON", "COMMENT":
			if value == "" {
				return nil, false
			}
		case "TITLE", "ARCID", "IFPLID", "ADEP", "ADES", "EOBD", "EOBT", "CTOD", "CTOT", "NEWCTOT", "TIMESTAMP", "REGCAUSE":
			if value == "" || fields[key] != "" {
				return nil, false
			}
			fields[key] = value
		default:
			return nil, false
		}
	}
	return fields, true
}

func adexpSlotTTO(parts []string) (int, bool) {
	var point, clock, level bool
	for i, part := range parts {
		key, value := splitADEXPSlotField(part)
		switch key {
		case "ADID", "PTID":
			if point || value == "" || strings.Contains(value, " ") || key == "ADID" && !aeroAlertAirport.MatchString(value) {
				return 0, false
			}
			point = true
		case "TO":
			if _, ok := parseADEXPSlotTime("1504", value); !ok || clock {
				return 0, false
			}
			clock = true
		case "FL":
			if level || value == "" || strings.Contains(value, " ") {
				return 0, false
			}
			level = true
		default:
			return i, point && clock
		}
	}
	return len(parts), point && clock
}

func adexpSlotAddressList(parts []string) (int, bool) {
	for i, part := range parts {
		key, value := splitADEXPSlotField(part)
		if key == "END" {
			return i + 1, i > 0 && value == "ADDR"
		}
		if key != "FAC" || value == "" || len(value) > 30 {
			return 0, false
		}
	}
	return 0, false
}

func splitADEXPSlotField(field string) (key, value string) {
	words := strings.Fields(field)
	if len(words) == 0 {
		return "", ""
	}
	return words[0], strings.Join(words[1:], " ")
}

func adexpSlotReason(code string) string {
	match := adexpSlotCause.FindStringSubmatch(code)
	if match == nil || match[1] == "G" && match[2] == "E" {
		return ""
	}
	causes := map[string]string{"C": "Air traffic control capacity", "W": "Weather", "G": "Airport capacity"}
	locations := map[string]string{"A": "during arrival", "D": "during departure", "E": "en route"}
	return causes[match[1]] + " " + locations[match[2]]
}
