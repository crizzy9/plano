package feeds

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const (
	maxTFMSBytes    = 2 << 20
	maxTFMSDepth    = 64
	maxTFMSGADVText = 8 << 10
	tfmsBaseNS      = "urn:us:gov:dot:faa:atm:tfm:"
	tfmsService     = tfmsBaseNS + "tfmdataservice"
	tfmsFlow        = tfmsBaseNS + "flowinformation"
	tfmsCommon      = tfmsBaseNS + "ficommonmessages"
	tfmsTypes       = tfmsBaseNS + "ficommondatatypes"
)

// ErrAdvisoryIgnored identifies a well-formed message with no supported advisory.
var ErrAdvisoryIgnored = errors.New("aviation message has no supported advisory")

type tfmsProgramSpec struct {
	payload, period, kind, label string
}

var tfmsPrograms = map[string]tfmsProgramSpec{
	"GS":  {"gsAdvisory", "groundStopPeriod", "ground_stop", "ground stop"},
	"GDP": {"gdpAdvisory", "arrivalsEstimatedFor", "ground_delay", "ground delay program"},
}

type tfmsProgram struct {
	messageType, status, airport, elementType, updated, expires, condition string
	starts, ends                                                           string
	general                                                                tfmsGeneralAdvisory
}

type tfmsGeneralAdvisory struct {
	number, origin, sent, title, text string
	textOversized                     bool
}

var (
	tfmsCancellationTitle = regexp.MustCompile(`^ATCSCC ADVZY ([0-9]{3}) ([A-Z][A-Z0-9]{2,3})/[A-Z][A-Z0-9]{2} ([0-9]{1,2}/[0-9]{1,2}/[0-9]{4}) CDM (GS CNX|GROUND STOP CNX|GROUND DELAY PROGRAM CNX)$`)
	tfmsCancellationRange = regexp.MustCompile(`^([0-9]{2}/[0-9]{4}Z)(?: ?- ?| )([0-9]{2}/[0-9]{4}Z)$`)
	tfmsEventTimeRange    = regexp.MustCompile(`^[0-9]{2}/[0-9]{4}Z?(?: ?- ?| )(?:[0-9]{2}/)?[0-9]{4}Z?$`)
)

// TFMSAdvisoryBodies converts one FAA TFMData Flow Information message from an
// operator's broker consumer to Plano's signed advisory envelope. Program
// definitions identify an airport, while individual flight assignment needs
// separate evidence.
func TFMSAdvisoryBodies(input io.Reader) ([][]byte, error) {
	program, err := readTFMSProgram(input)
	if err != nil {
		return nil, err
	}
	var advisory Advisory
	if program.messageType == "GADV" {
		advisory, err = program.general.cancellation()
	} else {
		advisory, err = program.definition()
	}
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(advisoryIngressEnvelope{Version: 1, Source: FAASWIM, Advisory: &advisory})
	if err != nil {
		return nil, err
	}
	return [][]byte{body}, nil
}

func (program tfmsProgram) definition() (Advisory, error) {
	spec, supported := tfmsPrograms[program.messageType]
	if !supported || program.status == "PROPOSED" {
		return Advisory{}, ErrAdvisoryIgnored
	}
	if program.status != "ACTUAL" || program.elementType != "APT" || !airportCode.MatchString(program.airport) {
		return Advisory{}, ErrInvalidQuery
	}
	observed, observedErr := time.Parse(time.RFC3339Nano, program.updated)
	start, startErr := time.Parse(time.RFC3339Nano, program.starts)
	end, endErr := time.Parse(time.RFC3339Nano, program.ends)
	if observedErr != nil || startErr != nil || endErr != nil || observed.IsZero() || start.IsZero() || end.IsZero() {
		return Advisory{}, ErrInvalidQuery
	}
	if program.expires != "" {
		expires, err := time.Parse(time.RFC3339Nano, program.expires)
		if err != nil || expires.IsZero() {
			return Advisory{}, ErrInvalidQuery
		}
		if expires.Before(end) {
			end = expires
		}
	}
	if !end.After(start) {
		return Advisory{}, ErrInvalidQuery
	}
	reason := "FAA SWIM reports a " + spec.label + " for selected flights to " + program.airport + "."
	if condition := advisoryPlainText(program.condition); condition != "" {
		reason += " Reported cause is " + strings.ToLower(condition) + "."
	}
	reason += " Check with your airline about your flight."
	return Advisory{
		ID:     "tfms/" + strings.ToLower(program.messageType) + "/" + program.airport,
		Source: FAASWIM, Kind: spec.kind, Airport: program.airport, Direction: "arrival",
		ObservedAt: observed.UTC(), StartsAt: new(start.UTC()), EndsAt: new(end.UTC()),
		Reason: reason, AirportContextOnly: true,
	}, nil
}

func (message tfmsGeneralAdvisory) cancellation() (Advisory, error) {
	title := tfmsCancellationTitle.FindStringSubmatch(strings.Join(strings.Fields(message.title), " "))
	if title == nil {
		return Advisory{}, ErrAdvisoryIgnored
	}
	number, airport := title[1], title[2]
	if message.textOversized || message.origin != "ATCSCC" || (message.number != number && message.number != "0"+number) {
		return Advisory{}, ErrInvalidQuery
	}
	observed, observedErr := time.Parse(time.RFC3339Nano, message.sent)
	issued, issuedErr := time.Parse("1/2/2006", title[3])
	observed = observed.UTC()
	if observedErr != nil || issuedErr != nil || observed.IsZero() || !observed.Truncate(24*time.Hour).Equal(issued) {
		return Advisory{}, ErrInvalidQuery
	}
	programType := "GS"
	if title[4] == "GROUND DELAY PROGRAM CNX" {
		programType = "GDP"
	}
	start, end, ok := tfmsCancelledPeriod(message.text, airport, programType, "CDM "+title[4], issued)
	if !ok {
		return Advisory{}, ErrInvalidQuery
	}
	key := strings.ToLower(programType) + "/" + airport
	return Advisory{
		ID:            "tfms/cnx/" + key + "/" + issued.Format("20060102") + "/" + number,
		TFMSProgramID: "tfms/" + key, Kind: AdvisoryTFMSCancellation, Source: FAASWIM,
		Airport: airport, Direction: "arrival", AirportContextOnly: true,
		ObservedAt: observed, StartsAt: &start, EndsAt: &end,
	}, nil
}

func tfmsCancelledPeriod(body, airport, programType, heading string, issued time.Time) (time.Time, time.Time, bool) {
	var lines []string
	for line := range strings.Lines(body) {
		line = strings.Join(strings.Fields(line), " ")
		if line == "COMMENTS" || strings.HasPrefix(line, "COMMENTS:") || strings.HasPrefix(line, "COMMENTS ") {
			break
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 0 {
		if event, ok := tfmsCancellationField(lines[0], "EVENT TIME"); ok {
			if !tfmsEventTimeRange.MatchString(event) {
				return time.Time{}, time.Time{}, false
			}
			lines = lines[1:]
		}
	}
	if len(lines) != 0 && lines[0] == heading {
		lines = lines[1:]
	}
	if len(lines) < 3 {
		return time.Time{}, time.Time{}, false
	}
	control, _ := tfmsCancellationField(lines[0], "CTL ELEMENT")
	element, _ := tfmsCancellationField(lines[1], "ELEMENT TYPE")
	if control != airport || element != "APT" {
		return time.Time{}, time.Time{}, false
	}
	lines = lines[2:]
	if adl, ok := tfmsCancellationField(lines[0], "ADL TIME"); ok {
		if _, err := time.Parse("1504Z", adl); err != nil {
			return time.Time{}, time.Time{}, false
		}
		lines = lines[1:]
	}
	label := programType + " CNX PERIOD"
	if len(lines) == 0 {
		return time.Time{}, time.Time{}, false
	}
	period, _ := tfmsCancellationField(lines[0], label)
	match := tfmsCancellationRange.FindStringSubmatch(period)
	if match == nil {
		return time.Time{}, time.Time{}, false
	}
	for _, line := range lines[1:] {
		for _, field := range [...]string{"CTL ELEMENT", "ELEMENT TYPE", "GS CNX PERIOD", "GDP CNX PERIOD", "CONSTRAINED FACILITIES", "RELEASED FACILITIES", "DEP FACILITIES INCLUDED", "DEPARTURE SCOPE", "FLT INCL"} {
			if _, ok := tfmsCancellationField(line, field); ok {
				return time.Time{}, time.Time{}, false
			}
		}
	}
	start, end := tfmsCancellationTime(match[1], issued), tfmsCancellationTime(match[2], issued)
	return start, end, !start.IsZero() && !end.IsZero() && end.After(start)
}

func tfmsCancellationField(line, label string) (string, bool) {
	value, ok := strings.CutPrefix(line, label)
	if !ok || value == "" || (value[0] != ' ' && value[0] != ':') {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), ":")), true
}

func tfmsCancellationTime(value string, issued time.Time) time.Time {
	clock, err := time.Parse("02/1504Z", value)
	if err != nil {
		return time.Time{}
	}
	// The body carries a day-of-month, while the title carries the issue date.
	for days := -2; days <= 2; days++ {
		date := issued.AddDate(0, 0, days)
		if date.Day() == clock.Day() {
			return date.Add(time.Duration(clock.Hour())*time.Hour + time.Duration(clock.Minute())*time.Minute)
		}
	}
	return time.Time{}
}

func readTFMSProgram(input io.Reader) (tfmsProgram, error) {
	raw, err := io.ReadAll(io.LimitReader(input, maxTFMSBytes+1))
	if err != nil {
		return tfmsProgram{}, fmt.Errorf("%w: could not read TFMData XML", ErrUnavailable)
	}
	if len(raw) > maxTFMSBytes {
		return tfmsProgram{}, fmt.Errorf("%w: TFMData XML exceeds %d bytes", ErrInvalidQuery, maxTFMSBytes)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	invalid := fmt.Errorf("%w: invalid TFMData Flow Information XML", ErrInvalidQuery)
	envelope := [...]xml.Name{
		{Space: tfmsService, Local: "tfmDataService"},
		{Space: tfmsService, Local: "fiOutput"},
		{Space: tfmsFlow, Local: "fiMessage"},
	}
	var program tfmsProgram
	var spec tfmsProgramSpec
	var path []xml.Name
	var wrappers [3]bool
	var declaration, payload, period bool
	var field *string
	var value strings.Builder
	seen := make(map[*string]bool)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return tfmsProgram{}, invalid
		}
		switch item := token.(type) {
		case xml.StartElement:
			depth := len(path)
			if field != nil || depth == maxTFMSDepth {
				return tfmsProgram{}, invalid
			}
			switch {
			case depth < len(envelope):
				if wrappers[depth] || item.Name != envelope[depth] {
					return tfmsProgram{}, invalid
				}
				wrappers[depth] = true
				if depth == 2 {
					program.messageType = tfmsMessageType(item.Attr)
					if program.messageType == "" {
						return tfmsProgram{}, invalid
					}
					spec = tfmsPrograms[program.messageType]
					if program.messageType == "GADV" {
						spec.payload = "generalAdvisory"
					}
				}
			case depth == 3:
				if payload || item.Name.Space != tfmsFlow || spec.payload != "" && item.Name.Local != spec.payload {
					return tfmsProgram{}, invalid
				}
				payload = true
			case depth == 4 && spec.payload != "":
				field = program.field(item.Name.Local)
				if item.Name.Local == spec.period {
					if period || item.Name.Space != tfmsCommon {
						return tfmsProgram{}, invalid
					}
					period = true
				}
				if field != nil && item.Name.Space != tfmsCommon {
					return tfmsProgram{}, invalid
				}
			case depth == 5 && period && path[4].Local == spec.period:
				switch item.Name.Local {
				case "startTime":
					field = &program.starts
				case "endTime":
					field = &program.ends
				}
				if field != nil && item.Name.Space != tfmsTypes {
					return tfmsProgram{}, invalid
				}
			}
			if field != nil {
				if seen[field] {
					return tfmsProgram{}, invalid
				}
				seen[field] = true
			}
			path = append(path, item.Name)
		case xml.EndElement:
			if field != nil {
				*field = strings.TrimSpace(value.String())
				if *field == "" && field != &program.condition && program.messageType != "GADV" {
					return tfmsProgram{}, invalid
				}
				field = nil
				value.Reset()
			}
			path = path[:len(path)-1]
		case xml.CharData:
			if field != nil {
				if field == &program.general.text {
					if value.Len()+len(item) > maxTFMSGADVText {
						program.general.textOversized = true
					}
					if program.general.textOversized {
						continue
					}
				} else if value.Len()+len(item) > 512 {
					return tfmsProgram{}, invalid
				}
				value.Write(item)
			} else if len(path) <= 3 || spec.payload != "" && (len(path) == 4 || len(path) == 5 && path[4].Local == spec.period) {
				if len(bytes.Trim(item, " \t\r\n")) != 0 {
					return tfmsProgram{}, invalid
				}
			}
		case xml.Directive:
			return tfmsProgram{}, invalid
		case xml.ProcInst:
			if item.Target != "xml" || declaration || wrappers[0] || !bytes.HasPrefix(raw, []byte("<?xml")) {
				return tfmsProgram{}, invalid
			}
			declaration = true
		}
	}
	if len(path) != 0 || !payload {
		return tfmsProgram{}, invalid
	}
	return program, nil
}

func tfmsMessageType(attrs []xml.Attr) string {
	var messageType string
	var found bool
	for _, attr := range attrs {
		if attr.Name != (xml.Name{Local: "msgType"}) {
			continue
		}
		if found {
			return ""
		}
		found, messageType = true, attr.Value
	}
	return messageType
}

func (p *tfmsProgram) field(name string) *string {
	if p.messageType == "GADV" {
		switch name {
		case "advisoryNumber":
			return &p.general.number
		case "origin":
			return &p.general.origin
		case "dateSent":
			return &p.general.sent
		case "advisoryTitle":
			return &p.general.title
		case "advisoryText":
			return &p.general.text
		default:
			return nil
		}
	}
	switch name {
	case "tmiStatus":
		return &p.status
	case "airportId":
		return &p.airport
	case "elementType":
		return &p.elementType
	case "updateTime":
		return &p.updated
	case "pgmExpTime":
		return &p.expires
	case "impactingCondition":
		return &p.condition
	default:
		return nil
	}
}
