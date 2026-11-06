package feeds

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const (
	MaxTFMSATFMSlotBytes = 64 << 10
	tfmsFlightData       = tfmsBaseNS + "flightdata"
	tfmsFlightCommon     = tfmsBaseNS + "flightdatacommonmessages"
	tfmsFlightCore       = tfmsBaseNS + "tfmdatacoreelements"
	tfmsSlotSchema       = "http://www.w3.org/2001/XMLSchema-instance"
)

var (
	errInvalidTFMSATFMSlot = fmt.Errorf("%w: invalid TFMData FlightControl slot", ErrInvalidQuery)
	tfmsSlotFlightRef      = regexp.MustCompile(`^[1-9][0-9]{0,11}$`)
	tfmsSlotAircraftID     = regexp.MustCompile(`^[A-Z0-9]{2,7}$`)
)

// TFMSATFMSlotBinding is the operator's independent match between a Plano
// provider instance and a TFMS flight reference and composite identity.
type TFMSATFMSlotBinding struct {
	Version            int       `json:"version"`
	ProviderFlightID   string    `json:"providerFlightId"`
	OriginICAO         string    `json:"originIcao"`
	DestinationICAO    string    `json:"destinationIcao"`
	FlightRef          string    `json:"flightRef"`
	AircraftID         string    `json:"aircraftId"`
	SourceOrigin       string    `json:"sourceOrigin"`
	SourceDestination  string    `json:"sourceDestination"`
	ScheduledDeparture time.Time `json:"scheduledDeparture"`
	IGTD               time.Time `json:"igtd"`
}

// DecodeTFMSATFMSlot accepts one legacy TFMData FlightControl report. The
// report's CTD is wheels-off time. Its IGTD only matches the TFMS identity.
func DecodeTFMSATFMSlot(input io.Reader, binding TFMSATFMSlotBinding, now time.Time) (ATFMSlot, error) {
	attrs, fields, err := readTFMSATFMSlot(input)
	if err != nil {
		return ATFMSlot{}, err
	}
	if binding.Version != 1 || !tfmsSlotFlightRef.MatchString(binding.FlightRef) || !tfmsSlotAircraftID.MatchString(binding.AircraftID) ||
		!validTFMSSlotAirport(binding.SourceOrigin, binding.OriginICAO) || !validTFMSSlotAirport(binding.SourceDestination, binding.DestinationICAO) ||
		attrs["flightRef"] != binding.FlightRef || attrs["acid"] != binding.AircraftID || fields[tfmsSlotAircraft] != binding.AircraftID ||
		!matchesTFMSSlotAirport(attrs["depArpt"], binding.SourceOrigin, binding.OriginICAO) ||
		!matchesTFMSSlotAirport(fields[tfmsSlotOrigin], binding.SourceOrigin, binding.OriginICAO) ||
		!matchesTFMSSlotAirport(attrs["arrArpt"], binding.SourceDestination, binding.DestinationICAO) ||
		!matchesTFMSSlotAirport(fields[tfmsSlotDestination], binding.SourceDestination, binding.DestinationICAO) {
		return ATFMSlot{}, errInvalidTFMSATFMSlot
	}
	igtd, err := time.Parse(time.RFC3339Nano, fields[tfmsSlotIGTD])
	if err != nil || igtd.IsZero() || !igtd.Equal(binding.IGTD) {
		return ATFMSlot{}, errInvalidTFMSATFMSlot
	}
	observed, err := time.Parse(time.RFC3339Nano, attrs["sourceTimeStamp"])
	if err != nil || observed.IsZero() {
		return ATFMSlot{}, errInvalidTFMSATFMSlot
	}
	var reason string
	switch fields[tfmsSlotProgram] {
	case "GDP":
		reason = "Ground delay program"
	case "AFP":
		reason = "Airspace flow program"
	case "CTOP":
		reason = "Collaborative trajectory options program"
	default:
		return ATFMSlot{}, errInvalidTFMSATFMSlot
	}
	slot := ATFMSlot{
		Version: 1, Source: FAASWIM, ProviderFlightID: binding.ProviderFlightID,
		OriginICAO: binding.OriginICAO, DestinationICAO: binding.DestinationICAO,
		ScheduledDeparture: binding.ScheduledDeparture, ObservedAt: observed,
	}
	switch fields[tfmsSlotIndicator] {
	case "CONTROL_ACTIVE":
		takeoff, err := time.Parse(time.RFC3339Nano, fields[tfmsSlotCTD])
		if err != nil || takeoff.IsZero() {
			return ATFMSlot{}, errInvalidTFMSATFMSlot
		}
		slot.Status, slot.TakeoffAt, slot.Reason = ATFMAssigned, &takeoff, reason
	case "CONTROL_CANCELED", "CONTROL_CANCELLED":
		slot.Status = ATFMWithdrawn
	default:
		return ATFMSlot{}, errInvalidTFMSATFMSlot
	}
	return normalizeATFMSlot(slot, now)
}

func validTFMSSlotAirport(source, icao string) bool {
	return aeroAlertAirport.MatchString(icao) && icao != "ZZZZ" && icao != "AFIL" &&
		(source == icao || len(source) == 3 && airportCode.MatchString(source))
}

func matchesTFMSSlotAirport(value, source, icao string) bool {
	return value == source || value == icao
}

type tfmsSlotElement int

const (
	tfmsSlotOutside tfmsSlotElement = iota
	tfmsSlotUnknown
	tfmsSlotService
	tfmsSlotOutput
	tfmsSlotMessage
	tfmsSlotControl
	tfmsSlotIdentity
	tfmsSlotDeparturePoint
	tfmsSlotArrivalPoint
	tfmsSlotControlData
	tfmsSlotAircraft
	tfmsSlotIGTD
	tfmsSlotOrigin
	tfmsSlotDestination
	tfmsSlotIndicator
	tfmsSlotCTD
	tfmsSlotProgram
)

var tfmsSlotElements = [...]struct {
	parent tfmsSlotElement
	name   xml.Name
}{
	tfmsSlotService:        {tfmsSlotOutside, xml.Name{Space: tfmsService, Local: "tfmDataService"}},
	tfmsSlotOutput:         {tfmsSlotService, xml.Name{Space: tfmsService, Local: "fltdOutput"}},
	tfmsSlotMessage:        {tfmsSlotOutput, xml.Name{Space: tfmsFlightData, Local: "fltdMessage"}},
	tfmsSlotControl:        {tfmsSlotMessage, xml.Name{Space: tfmsFlightData, Local: "ncsmFlightControl"}},
	tfmsSlotIdentity:       {tfmsSlotControl, xml.Name{Space: tfmsFlightCommon, Local: "qualifiedAircraftId"}},
	tfmsSlotDeparturePoint: {tfmsSlotIdentity, xml.Name{Space: tfmsFlightCore, Local: "departurePoint"}},
	tfmsSlotArrivalPoint:   {tfmsSlotIdentity, xml.Name{Space: tfmsFlightCore, Local: "arrivalPoint"}},
	tfmsSlotControlData:    {tfmsSlotControl, xml.Name{Space: tfmsFlightCommon, Local: "ncsmControlData"}},
	tfmsSlotAircraft:       {tfmsSlotIdentity, xml.Name{Space: tfmsFlightCore, Local: "aircraftId"}},
	tfmsSlotIGTD:           {tfmsSlotIdentity, xml.Name{Space: tfmsFlightCore, Local: "igtd"}},
	tfmsSlotOrigin:         {tfmsSlotDeparturePoint, xml.Name{Space: tfmsFlightCore, Local: "airport"}},
	tfmsSlotDestination:    {tfmsSlotArrivalPoint, xml.Name{Space: tfmsFlightCore, Local: "airport"}},
	tfmsSlotIndicator:      {tfmsSlotControl, xml.Name{Space: tfmsFlightCommon, Local: "controlIndicator"}},
	tfmsSlotCTD:            {tfmsSlotControlData, xml.Name{Space: tfmsFlightCommon, Local: "ctd"}},
	tfmsSlotProgram:        {tfmsSlotControlData, xml.Name{Space: tfmsFlightCommon, Local: "controlProgramType"}},
}

func tfmsSlotChild(parent tfmsSlotElement, name xml.Name) (tfmsSlotElement, bool) {
	// Ancillary fields can be ignored. A reserved name outside its qualified
	// path would make the identity or control state ambiguous.
	reserved := false
	for element, spec := range tfmsSlotElements {
		if spec.name.Local == name.Local {
			reserved = true
			if spec.parent == parent && spec.name == name {
				return tfmsSlotElement(element), true
			}
		}
	}
	ancillaryParent := parent == tfmsSlotUnknown || parent >= tfmsSlotControl && parent < tfmsSlotAircraft
	return tfmsSlotUnknown, ancillaryParent && !reserved
}

func tfmsSlotHeader(attributes []xml.Attr) (map[string]string, error) {
	attrs := make(map[string]string)
	for _, attr := range attributes {
		switch attr.Name.Local {
		case "msgType", "fdTrigger", "sourceFacility", "sourceTimeStamp", "flightRef", "acid", "depArpt", "arrArpt", "sensitivity", "sensReason":
			if _, duplicate := attrs[attr.Name.Local]; duplicate || attr.Name.Space != "" {
				return nil, errInvalidTFMSATFMSlot
			}
			attrs[attr.Name.Local] = attr.Value
		}
	}
	if attrs["sensitivity"] == "R" || attrs["sensReason"] != "" {
		return nil, ErrNotFound
	}
	if attrs["msgType"] != "FlightControl" || attrs["fdTrigger"] != "TMI_UPDATE" || attrs["sourceFacility"] != "TFMS" || attrs["sensitivity"] != "A" {
		return nil, errInvalidTFMSATFMSlot
	}
	return attrs, nil
}

func tfmsSlotNilAttribute(attributes []xml.Attr) (nilValue, valid bool) {
	var found bool
	for _, attr := range attributes {
		if attr.Name != (xml.Name{Space: tfmsSlotSchema, Local: "nil"}) {
			continue
		}
		if found {
			return false, false
		}
		found = true
		switch attr.Value {
		case "true", "1":
			nilValue = true
		case "false", "0":
		default:
			return false, false
		}
	}
	return nilValue, true
}

func readTFMSATFMSlot(input io.Reader) (map[string]string, map[tfmsSlotElement]string, error) {
	raw, err := io.ReadAll(io.LimitReader(input, MaxTFMSATFMSlotBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: could not read TFMData FlightControl", ErrUnavailable)
	}
	if len(raw) > MaxTFMSATFMSlotBytes {
		return nil, nil, errInvalidTFMSATFMSlot
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	path := []tfmsSlotElement{tfmsSlotOutside}
	var attrs map[string]string
	fields := make(map[tfmsSlotElement]string)
	var seen [len(tfmsSlotElements)]bool
	var declaration, nilCTD bool
	var value strings.Builder
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, errInvalidTFMSATFMSlot
		}
		parent := path[len(path)-1]
		switch item := token.(type) {
		case xml.StartElement:
			element, ok := tfmsSlotChild(parent, item.Name)
			if !ok || len(path) > maxTFMSDepth || element != tfmsSlotUnknown && seen[element] {
				return nil, nil, errInvalidTFMSATFMSlot
			}
			if element != tfmsSlotUnknown {
				seen[element] = true
			}
			if element == tfmsSlotMessage {
				attrs, err = tfmsSlotHeader(item.Attr)
				if err != nil {
					return nil, nil, err
				}
			}
			if element != tfmsSlotUnknown {
				nilValue, valid := tfmsSlotNilAttribute(item.Attr)
				if !valid || nilValue && element != tfmsSlotCTD {
					return nil, nil, errInvalidTFMSATFMSlot
				}
				if element == tfmsSlotCTD {
					nilCTD = nilValue
				}
			}
			path = append(path, element)
		case xml.EndElement:
			if parent >= tfmsSlotAircraft {
				scalar := strings.TrimSpace(value.String())
				if parent == tfmsSlotCTD && nilCTD && scalar != "" {
					return nil, nil, errInvalidTFMSATFMSlot
				}
				fields[parent] = scalar
				value.Reset()
			}
			path = path[:len(path)-1]
		case xml.CharData:
			if parent >= tfmsSlotAircraft {
				if value.Len()+len(item) > 512 {
					return nil, nil, errInvalidTFMSATFMSlot
				}
				value.Write(item)
			} else if parent != tfmsSlotUnknown && len(bytes.Trim(item, " \t\r\n")) != 0 {
				return nil, nil, errInvalidTFMSATFMSlot
			}
		case xml.Directive:
			return nil, nil, errInvalidTFMSATFMSlot
		case xml.ProcInst:
			if item.Target != "xml" || declaration || seen[tfmsSlotService] || !bytes.HasPrefix(raw, []byte("<?xml")) {
				return nil, nil, errInvalidTFMSATFMSlot
			}
			declaration = true
		}
	}
	if len(path) != 1 || !seen[tfmsSlotControl] {
		return nil, nil, errInvalidTFMSATFMSlot
	}
	return attrs, fields, nil
}
