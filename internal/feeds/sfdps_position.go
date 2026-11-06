package feeds

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxSFDPSPositionBytes = 256 << 10
	maxSFDPSDepth         = 48
	maxSFDPSNodes         = 8192
	maxSFDPSMessages      = 64
	sfdpsNAS              = "http://www.faa.aero/nas/3.0"
	sfdpsFIXMFlight       = "http://www.fixm.aero/flight/3.0"
	sfdpsFIXMBase         = "http://www.fixm.aero/base/3.0"
	sfdpsXSI              = "http://www.w3.org/2001/XMLSchema-instance"
	sfdpsXML              = "http://www.w3.org/XML/1998/namespace"
	sfdpsXMLNS            = "http://www.w3.org/2000/xmlns/"
	sfdpsWGS84            = "urn:ogc:def:crs:EPSG::4326"
)

var (
	errInvalidSFDPSPosition = fmt.Errorf("%w: invalid SFDPS FIXM-NAS position report", ErrInvalidQuery)
	sfdpsGUFI               = regexp.MustCompile(`(?i)^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	sfdpsAircraftID         = regexp.MustCompile(`^[A-Z0-9]{2,7}$`)
)

// SFDPSPositionBinding is the operator's independent match between a source
// GUFI and an exact Plano provider flight that may display its positions.
type SFDPSPositionBinding struct {
	Version            int       `json:"version"`
	DisplayAllowed     bool      `json:"displayAllowed"`
	ProviderFlightID   string    `json:"providerFlightId"`
	OriginICAO         string    `json:"originICAO"`
	DestinationICAO    string    `json:"destinationICAO"`
	TailNumber         string    `json:"tailNumber,omitempty"`
	ScheduledDeparture time.Time `json:"scheduledDeparture"`
	ActualTakeoff      time.Time `json:"actualTakeoff"`
	GUFI               string    `json:"gufi"`
	AircraftID         string    `json:"aircraftId"`
}

// DecodeSFDPSPositions accepts the FAA SFDPS FIXM-NAS 3.0 track profile. It
// uses the reported track position and time from a surveillance TH message.
// A valid document without a matching, usable observation returns ErrNotFound.
func DecodeSFDPSPositions(input io.Reader, binding SFDPSPositionBinding, now time.Time) (RadarPositionReport, error) {
	if binding.Version != 1 || !binding.DisplayAllowed || !providerIdentifier.MatchString(binding.ProviderFlightID) ||
		!aeroAlertAirport.MatchString(binding.OriginICAO) || !aeroAlertAirport.MatchString(binding.DestinationICAO) ||
		!validRadarTailNumber(binding.TailNumber) ||
		binding.OriginICAO == "ZZZZ" || binding.OriginICAO == "AFIL" || binding.DestinationICAO == "ZZZZ" || binding.DestinationICAO == "AFIL" ||
		!sfdpsGUFI.MatchString(binding.GUFI) || binding.GUFI == "00000000-0000-0000-0000-000000000000" || !sfdpsAircraftID.MatchString(binding.AircraftID) ||
		binding.ScheduledDeparture.Year() < 2000 || binding.ScheduledDeparture.Year() > 2100 ||
		binding.ActualTakeoff.Year() < 2000 || binding.ActualTakeoff.Year() > 2100 || binding.ActualTakeoff.After(now) {
		return RadarPositionReport{}, errInvalidSFDPSPosition
	}
	root, err := readSFDPSXML(input)
	if err != nil {
		return RadarPositionReport{}, err
	}
	messages, err := sfdpsFlightMessages(root)
	if err != nil {
		return RadarPositionReport{}, err
	}
	fixes := make(map[time.Time]Position)
	for _, message := range messages {
		fix, err := sfdpsTrackPosition(message, binding, now)
		if err != nil {
			return RadarPositionReport{}, err
		}
		if fix == nil {
			continue
		}
		if previous, exists := fixes[fix.At]; exists {
			if previous.Latitude != fix.Latitude || previous.Longitude != fix.Longitude ||
				!sameRadarMeasurement(previous.AltitudeFeet, fix.AltitudeFeet) || !sameRadarMeasurement(previous.GroundSpeedKnots, fix.GroundSpeedKnots) {
				return RadarPositionReport{}, errInvalidSFDPSPosition
			}
			continue
		}
		if len(fixes) == maxRadarPositionFixes {
			return RadarPositionReport{}, errInvalidSFDPSPosition
		}
		fixes[fix.At] = *fix
	}
	if len(fixes) == 0 {
		return RadarPositionReport{}, ErrNotFound
	}
	positions := make([]Position, 0, len(fixes))
	for _, fix := range fixes {
		positions = append(positions, fix)
	}
	slices.SortFunc(positions, func(a, b Position) int { return a.At.Compare(b.At) })
	return RadarPositionReport{
		ProviderFlightID: binding.ProviderFlightID, OriginICAO: binding.OriginICAO, DestinationICAO: binding.DestinationICAO,
		TailNumber:         binding.TailNumber,
		ScheduledDeparture: binding.ScheduledDeparture.UTC(), ActualTakeoff: binding.ActualTakeoff.UTC(), Positions: positions,
	}, nil
}

type sfdpsXMLNode struct {
	start      xml.StartElement
	schemaType xml.Name
	children   []*sfdpsXMLNode
	text       strings.Builder
}

func readSFDPSXML(input io.Reader) (*sfdpsXMLNode, error) {
	raw, err := io.ReadAll(io.LimitReader(input, maxSFDPSPositionBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: could not read SFDPS position XML", ErrUnavailable)
	}
	if len(raw) > maxSFDPSPositionBytes || !utf8.Valid(raw) {
		return nil, errInvalidSFDPSPosition
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	var root *sfdpsXMLNode
	var path []*sfdpsXMLNode
	var declaration bool
	nodes := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errInvalidSFDPSPosition
		}
		switch item := token.(type) {
		case xml.StartElement:
			if len(path) == maxSFDPSDepth || nodes == maxSFDPSNodes {
				return nil, errInvalidSFDPSPosition
			}
			nodes++
			node := &sfdpsXMLNode{start: item.Copy()}
			if len(path) == 0 {
				if root != nil {
					return nil, errInvalidSFDPSPosition
				}
				root = node
			} else {
				parent := path[len(path)-1]
				parent.children = append(parent.children, node)
			}
			path = append(path, node)
			seen := make(map[xml.Name]bool, len(item.Attr))
			for _, attribute := range item.Attr {
				if seen[attribute.Name] || !validSFDPSNamespace(attribute) {
					return nil, errInvalidSFDPSPosition
				}
				seen[attribute.Name] = true
				if attribute.Name == (xml.Name{Space: sfdpsXSI, Local: "type"}) {
					node.schemaType = resolveSFDPSQName(attribute.Value, path)
					if node.schemaType.Local == "" {
						return nil, errInvalidSFDPSPosition
					}
				}
			}
		case xml.EndElement:
			path = path[:len(path)-1]
		case xml.CharData:
			if len(path) != 0 {
				path[len(path)-1].text.Write(item)
			} else if len(bytes.TrimSpace(item)) != 0 {
				return nil, errInvalidSFDPSPosition
			}
		case xml.Directive:
			return nil, errInvalidSFDPSPosition
		case xml.ProcInst:
			if item.Target != "xml" || declaration || root != nil || !bytes.HasPrefix(raw, []byte("<?xml")) {
				return nil, errInvalidSFDPSPosition
			}
			declaration = true
		}
	}
	if root == nil || len(path) != 0 {
		return nil, errInvalidSFDPSPosition
	}
	return root, nil
}

func validSFDPSNamespace(attr xml.Attr) bool {
	var prefix string
	switch {
	case attr.Name.Space == "xmlns":
		prefix = attr.Name.Local
	case attr.Name != (xml.Name{Local: "xmlns"}):
		return true
	}
	// In encoding/xml a namespace URI of "xmlns" looks like a namespace
	// declaration, and a prefix bound to the empty URI looks unqualified.
	if prefix == "xmlns" || attr.Value == "xmlns" || attr.Value == sfdpsXMLNS {
		return false
	}
	if prefix == "xml" || attr.Value == sfdpsXML {
		return prefix == "xml" && attr.Value == sfdpsXML
	}
	return prefix == "" || attr.Value != ""
}

func resolveSFDPSQName(value string, path []*sfdpsXMLNode) xml.Name {
	prefix, local, prefixed := strings.Cut(strings.TrimSpace(value), ":")
	declaration := xml.Name{Space: "xmlns", Local: prefix}
	if !prefixed {
		local, declaration = prefix, xml.Name{Local: "xmlns"}
	}
	if local == "" || prefixed && prefix == "" || strings.ContainsAny(local, ": \t\r\n") {
		return xml.Name{}
	}
	for i := len(path) - 1; i >= 0; i-- {
		for _, attr := range path[i].start.Attr {
			if attr.Name == declaration {
				return xml.Name{Space: attr.Value, Local: local}
			}
		}
	}
	if prefixed {
		return xml.Name{}
	}
	return xml.Name{Local: local}
}

func sfdpsFlightMessages(root *sfdpsXMLNode) ([]*sfdpsXMLNode, error) {
	if root.start.Name.Space != sfdpsNAS || root.isNil() || strings.TrimSpace(root.text.String()) != "" {
		return nil, errInvalidSFDPSPosition
	}
	switch root.start.Name.Local {
	case "FlightMessage":
		if root.schemaType != (xml.Name{}) && root.schemaType != (xml.Name{Space: sfdpsNAS, Local: "FlightMessageType"}) {
			return nil, errInvalidSFDPSPosition
		}
		return []*sfdpsXMLNode{root}, nil
	case "MessageCollection":
		if len(root.children) == 0 || len(root.children) > maxSFDPSMessages ||
			root.schemaType != (xml.Name{}) && root.schemaType != (xml.Name{Space: sfdpsNAS, Local: "MessageCollectionType"}) {
			return nil, errInvalidSFDPSPosition
		}
		var messages []*sfdpsXMLNode
		for _, child := range root.children {
			if child.start.Name != (xml.Name{Local: "message"}) || child.schemaType.Local == "" {
				return nil, errInvalidSFDPSPosition
			}
			if child.schemaType == (xml.Name{Space: sfdpsNAS, Local: "FlightMessageType"}) {
				messages = append(messages, child)
			}
		}
		return messages, nil
	default:
		return nil, errInvalidSFDPSPosition
	}
}

// Field selection is local and path-scoped. A foreign or duplicated element
// at a selected path invalidates the report instead of overriding a field.
type sfdpsFields struct{ invalid bool }

func (f *sfdpsFields) child(parent *sfdpsXMLNode, local string) *sfdpsXMLNode {
	if parent == nil {
		return nil
	}
	if parent.isNil() || strings.TrimSpace(parent.text.String()) != "" {
		f.invalid = true
		return nil
	}
	var selected *sfdpsXMLNode
	for _, child := range parent.children {
		if child.start.Name.Local != local {
			continue
		}
		if child.start.Name.Space != "" || child.isNil() || selected != nil {
			f.invalid = true
			return nil
		}
		selected = child
	}
	return selected
}

func (f *sfdpsFields) attribute(node *sfdpsXMLNode, local string) (string, bool) {
	if node == nil {
		return "", false
	}
	var value string
	var present bool
	for _, attr := range node.start.Attr {
		if attr.Name.Local != local || attr.Name.Space == "xmlns" {
			continue
		}
		if attr.Name.Space != "" || present {
			f.invalid = true
		}
		value, present = attr.Value, true
	}
	return value, present
}

func (f *sfdpsFields) scalar(node *sfdpsXMLNode) string {
	if node == nil {
		return ""
	}
	if len(node.children) != 0 {
		f.invalid = true
	}
	return strings.TrimSpace(node.text.String())
}

func (n *sfdpsXMLNode) isNil() bool {
	for _, attr := range n.start.Attr {
		if attr.Name == (xml.Name{Space: sfdpsXSI, Local: "nil"}) {
			return attr.Value != "false" && attr.Value != "0"
		}
	}
	return false
}

func (n *sfdpsXMLNode) hasOptionalType(allowed ...xml.Name) bool {
	return n == nil || n.schemaType == (xml.Name{}) || slices.Contains(allowed, n.schemaType)
}

func sfdpsTrackPosition(message *sfdpsXMLNode, binding SFDPSPositionBinding, now time.Time) (*Position, error) {
	var fields sfdpsFields
	flight := fields.child(message, "flight")
	source, _ := fields.attribute(flight, "source")
	if flight == nil || fields.invalid {
		return nil, errInvalidSFDPSPosition
	}
	if source != "TH" {
		return nil, nil
	}
	if flight.schemaType != (xml.Name{Space: sfdpsNAS, Local: "NasFlightType"}) {
		return nil, errInvalidSFDPSPosition
	}
	gufi := fields.child(flight, "gufi")
	codeSpace, _ := fields.attribute(gufi, "codeSpace")
	identity := fields.scalar(gufi)
	if fields.invalid {
		return nil, errInvalidSFDPSPosition
	}
	if codeSpace != "urn:uuid" || !strings.EqualFold(identity, binding.GUFI) {
		return nil, nil
	}
	aircraft, _ := fields.attribute(fields.child(flight, "flightIdentification"), "aircraftIdentification")
	origin, _ := fields.attribute(fields.child(flight, "departure"), "departurePoint")
	destination, _ := fields.attribute(fields.child(flight, "arrival"), "arrivalPoint")
	status, _ := fields.attribute(fields.child(flight, "flightStatus"), "fdpsFlightStatus")
	if fields.invalid {
		return nil, errInvalidSFDPSPosition
	}
	if aircraft != binding.AircraftID || origin != binding.OriginICAO || destination != binding.DestinationICAO || status != "ACTIVE" {
		return nil, nil
	}
	if binding.TailNumber != "" {
		// The SFDPS FIXM mapping puts the optional REG indicator here.
		registration, _ := fields.attribute(fields.child(flight, "aircraftDescription"), "registration")
		if fields.invalid {
			return nil, errInvalidSFDPSPosition
		}
		if registration = strings.TrimSpace(registration); registration != "" && !strings.EqualFold(registration, binding.TailNumber) {
			return nil, nil
		}
	}
	enRoute := fields.child(flight, "enRoute")
	track := fields.child(enRoute, "position")
	reportSource, _ := fields.attribute(track, "reportSource")
	stamp, _ := fields.attribute(track, "positionTime")
	_, coasting := fields.attribute(track, "coastIndicator")
	if fields.invalid || !enRoute.hasOptionalType(xml.Name{Space: sfdpsNAS, Local: "NasEnRouteType"}, xml.Name{Space: sfdpsFIXMFlight, Local: "EnRouteType"}) ||
		!track.hasOptionalType(xml.Name{Space: sfdpsNAS, Local: "NasAircraftPositionType"}, xml.Name{Space: sfdpsFIXMFlight, Local: "AircraftPositionType"}) {
		return nil, errInvalidSFDPSPosition
	}
	if reportSource != "SURVEILLANCE" || coasting || stamp == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return nil, errInvalidSFDPSPosition
	}
	if at.Before(binding.ActualTakeoff) || at.Before(now.Add(-RadarPositionFreshness)) || at.After(now) {
		return nil, nil
	}
	reported := fields.child(track, "position")
	location := fields.child(reported, "location")
	crs, _ := fields.attribute(location, "srsName")
	coordinates := fields.child(location, "pos")
	if fields.invalid || !reported.hasOptionalType(xml.Name{Space: sfdpsFIXMBase, Local: "LocationPointType"}) {
		return nil, errInvalidSFDPSPosition
	}
	if coordinates == nil {
		return nil, nil
	}
	components := strings.Fields(fields.scalar(coordinates))
	if crs != sfdpsWGS84 || len(components) != 2 {
		return nil, errInvalidSFDPSPosition
	}
	latitude := fields.number(components[0], -90, 90)
	longitude := fields.number(components[1], -180, 180)
	altitude := fields.measurement(fields.child(track, "altitude"), "FEET", -2000, 100000)
	speed := fields.measurement(fields.child(fields.child(track, "actualSpeed"), "surveillance"), "KNOTS", 0, 2500)
	if fields.invalid {
		return nil, errInvalidSFDPSPosition
	}
	return &Position{
		Latitude: *latitude, Longitude: *longitude, AltitudeFeet: altitude, GroundSpeedKnots: speed,
		At: at.UTC(), Source: FAASWIM, Kind: "radar",
	}, nil
}

func (f *sfdpsFields) measurement(node *sfdpsXMLNode, unit string, low, high float64) *float64 {
	if node == nil {
		return nil
	}
	actual, _ := f.attribute(node, "uom")
	if actual != unit {
		f.invalid = true
		return nil
	}
	return f.number(f.scalar(node), low, high)
}

func (f *sfdpsFields) number(value string, low, high float64) *float64 {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || strings.ContainsAny(value, "_xXpP") || !radarPositionRange(&number, low, high) {
		f.invalid = true
		return nil
	}
	return &number
}
