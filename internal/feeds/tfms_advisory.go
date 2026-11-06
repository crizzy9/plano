package feeds

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	maxTFMSBytes = 2 << 20
	maxTFMSDepth = 64
	tfmsBaseNS   = "urn:us:gov:dot:faa:atm:tfm:"
	tfmsService  = tfmsBaseNS + "tfmdataservice"
	tfmsFlow     = tfmsBaseNS + "flowinformation"
	tfmsCommon   = tfmsBaseNS + "ficommonmessages"
	tfmsTypes    = tfmsBaseNS + "ficommondatatypes"
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
}

// TFMSAdvisoryBodies converts one FAA TFMData Flow Information message from an
// operator's broker consumer to Plano's signed advisory envelope. Program
// definitions identify an airport, while individual flight assignment needs
// separate evidence.
func TFMSAdvisoryBodies(input io.Reader) ([][]byte, error) {
	program, err := readTFMSProgram(input)
	if err != nil {
		return nil, err
	}
	spec, supported := tfmsPrograms[program.messageType]
	if !supported || program.status == "PROPOSED" {
		return nil, ErrAdvisoryIgnored
	}
	if program.status != "ACTUAL" || program.elementType != "APT" || !airportCode.MatchString(program.airport) {
		return nil, ErrInvalidQuery
	}
	observed, observedErr := time.Parse(time.RFC3339Nano, program.updated)
	start, startErr := time.Parse(time.RFC3339Nano, program.starts)
	end, endErr := time.Parse(time.RFC3339Nano, program.ends)
	if observedErr != nil || startErr != nil || endErr != nil || observed.IsZero() || start.IsZero() || end.IsZero() {
		return nil, ErrInvalidQuery
	}
	if program.expires != "" {
		expires, err := time.Parse(time.RFC3339Nano, program.expires)
		if err != nil || expires.IsZero() {
			return nil, ErrInvalidQuery
		}
		if expires.Before(end) {
			end = expires
		}
	}
	if !end.After(start) {
		return nil, ErrInvalidQuery
	}
	reason := "FAA SWIM reports a " + spec.label + " for selected flights to " + program.airport + "."
	if condition := advisoryPlainText(program.condition); condition != "" {
		reason += " Reported cause is " + strings.ToLower(condition) + "."
	}
	reason += " Check with your airline about your flight."
	advisory := Advisory{
		ID:     "tfms/" + strings.ToLower(program.messageType) + "/" + program.airport,
		Source: FAASWIM, Kind: spec.kind, Airport: program.airport, Direction: "arrival",
		ObservedAt: observed.UTC(), StartsAt: new(start.UTC()), EndsAt: new(end.UTC()),
		Reason: reason, AirportContextOnly: true,
	}
	body, err := json.Marshal(advisoryIngressEnvelope{Version: 1, Source: FAASWIM, Advisory: &advisory})
	if err != nil {
		return nil, err
	}
	return [][]byte{body}, nil
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
				if *field == "" && field != &program.condition {
					return tfmsProgram{}, invalid
				}
				field = nil
				value.Reset()
			}
			path = path[:len(path)-1]
		case xml.CharData:
			if field != nil {
				if value.Len()+len(item) > 512 {
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
