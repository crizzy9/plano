package feeds

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strings"
)

const (
	maxAIXMBytes  = 1 << 20
	maxAIXMDepth  = 64
	maxAIXMNOTAMs = 128
	aixm51        = "http://www.aixm.aero/schema/5.1"
	aixm511       = "http://www.aixm.aero/schema/5.1.1"
	xlinkNS       = "http://www.w3.org/1999/xlink"
)

type aixmNOTAM struct {
	Series, Number, Year, Type                   string
	ReferredSeries, ReferredNumber, ReferredYear string
	Issued, Scope, Location                      string
	EffectiveStart, EffectiveEnd                 string
	EstimatedEnd, Permanent                      string
	Schedule, Text, LowerLimit, UpperLimit       string
	MinimumFL, MaximumFL                         string
	Traffic, SelectionCode                       string
	Publisher                                    string
}

// readAIXMNOTAMs extracts local Event-extension notifications. XML attributes
// that reference other documents remain data and never trigger a lookup.
func readAIXMNOTAMs(input io.Reader) ([]aixmNOTAM, error) {
	raw, err := io.ReadAll(io.LimitReader(input, maxAIXMBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: could not read AIXM XML", ErrUnavailable)
	}
	if len(raw) > maxAIXMBytes {
		return nil, fmt.Errorf("%w: AIXM XML exceeds %d bytes", ErrInvalidQuery, maxAIXMBytes)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	invalid := fmt.Errorf("%w: invalid AIXM NOTAM XML", ErrInvalidQuery)

	var path, noticePath []xml.Name
	var notices []aixmNOTAM
	var notice *aixmNOTAM
	var seen map[*string]bool
	var field *string
	var value strings.Builder
	var publisherRef *string
	var declaration bool
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, invalid
		}
		switch item := token.(type) {
		case xml.StartElement:
			if field != nil || len(path) == maxAIXMDepth {
				return nil, invalid
			}
			if len(path) == 0 {
				if noticePath != nil {
					return nil, invalid
				}
				noticePath = aixmNoticePath(item.Name)
				if noticePath == nil {
					return nil, invalid
				}
			}
			path = append(path, item.Name)
			if slices.Equal(path, noticePath) {
				if len(notices) == maxAIXMNOTAMs {
					return nil, fmt.Errorf("%w: too many AIXM NOTAM records", ErrInvalidQuery)
				}
				notice = new(aixmNOTAM)
				seen = make(map[*string]bool)
			} else if notice != nil && len(path) == len(noticePath)+1 {
				field = notice.scalar(item.Name.Local)
				if field != nil {
					if seen[field] || item.Name.Space != noticePath[len(noticePath)-1].Space {
						return nil, invalid
					}
					seen[field] = true
					if field == &notice.Publisher {
						for _, attr := range item.Attr {
							if attr.Name == (xml.Name{Space: xlinkNS, Local: "href"}) {
								if publisherRef != nil {
									return nil, invalid
								}
								publisherRef = &attr.Value
							}
						}
					}
				}
			}
		case xml.EndElement:
			if field != nil {
				content := strings.TrimSpace(value.String())
				if publisherRef != nil {
					if content != "" {
						return nil, invalid
					}
					content = strings.TrimSpace(*publisherRef)
				}
				*field = content
				field, publisherRef = nil, nil
				value.Reset()
			} else if notice != nil && len(path) == len(noticePath) {
				if notice.Series == "" || notice.Number == "" || notice.Year == "" || notice.Type == "" {
					return nil, invalid
				}
				notices = append(notices, *notice)
				notice = nil
			}
			path = path[:len(path)-1]
		case xml.CharData:
			if field != nil {
				value.Write(item)
			} else if len(path) == 0 || len(path) <= len(noticePath) && slices.Equal(path, noticePath[:len(path)]) {
				if len(bytes.Trim(item, " \t\r\n")) != 0 {
					return nil, invalid
				}
			}
		case xml.Directive:
			return nil, invalid
		case xml.ProcInst:
			if item.Target != "xml" || declaration || noticePath != nil || !bytes.HasPrefix(raw, []byte("<?xml")) {
				return nil, invalid
			}
			declaration = true
		}
	}
	if len(path) != 0 || len(notices) == 0 {
		return nil, invalid
	}
	return notices, nil
}

func aixmNoticePath(root xml.Name) []xml.Name {
	for _, version := range []struct{ base, notification string }{
		{aixm51, "textNOTAM"},
		{aixm511, "notification"},
	} {
		event := version.base + "/event"
		message := version.base + "/message"
		notice := xml.Name{Space: event, Local: "NOTAM"}
		if root == notice {
			return []xml.Name{root}
		}
		if root == (xml.Name{Space: message, Local: "AIXMBasicMessage"}) {
			return []xml.Name{
				root,
				{Space: message, Local: "hasMember"},
				{Space: event, Local: "Event"},
				{Space: event, Local: "timeSlice"},
				{Space: event, Local: "EventTimeSlice"},
				{Space: event, Local: version.notification},
				notice,
			}
		}
	}
	return nil
}

func (n *aixmNOTAM) scalar(name string) *string {
	switch name {
	case "series":
		return &n.Series
	case "number":
		return &n.Number
	case "year":
		return &n.Year
	case "type":
		return &n.Type
	case "referredSeries":
		return &n.ReferredSeries
	case "referredNumber":
		return &n.ReferredNumber
	case "referredYear":
		return &n.ReferredYear
	case "issued":
		return &n.Issued
	case "scope":
		return &n.Scope
	case "location":
		return &n.Location
	case "effectiveStart":
		return &n.EffectiveStart
	case "effectiveEnd":
		return &n.EffectiveEnd
	case "estimatedEnd":
		return &n.EstimatedEnd
	case "permanent":
		return &n.Permanent
	case "schedule":
		return &n.Schedule
	case "text":
		return &n.Text
	case "lowerLimit":
		return &n.LowerLimit
	case "upperLimit":
		return &n.UpperLimit
	case "minimumFL":
		return &n.MinimumFL
	case "maximumFL":
		return &n.MaximumFL
	case "traffic":
		return &n.Traffic
	case "selectionCode":
		return &n.SelectionCode
	case "publisher", "publisherNOF":
		return &n.Publisher
	default:
		return nil
	}
}
