package itinerary

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"slices"
	"strings"
	"unicode/utf8"
)

const MaxEmailBytes = 256 << 10

type emailParts struct {
	plain, html []emailTextPart
	calendars   []emailCalendarPart
	count       int
}

type emailTextPart struct {
	text        string
	alternative int
}

type emailCalendarPart struct {
	emailTextPart
	forceCancel bool
}

// Email extracts itinerary facts from RFC 5322 mail. Attachments other than
// calendar invitations are ignored and links are never fetched.
func Email(raw []byte) (Itinerary, error) {
	if len(raw) > MaxEmailBytes {
		return Itinerary{}, ErrInvalid
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Itinerary{}, ErrInvalid
	}
	var parts emailParts
	parts.read(message.Header.Get, message.Body, 0, 0)
	decoder := mime.WordDecoder{}
	subject, err := decoder.DecodeHeader(message.Header.Get("Subject"))
	if err != nil {
		subject = ""
	}
	var cancellations calendarCancellations
	inputs := make([]calendarInput, len(parts.calendars))
	cancelledAlternatives := make(map[int]bool)
	for i, part := range parts.calendars {
		inputs[i] = readCalendar(part.text, part.forceCancel)
		if inputs[i].collectCancellations(&cancellations) && part.alternative != 0 {
			cancelledAlternatives[part.alternative] = true
		}
	}
	localDepartures := make(map[Leg]bool)
	var confirmed Itinerary
	var positiveParts int
	for i, input := range inputs {
		extra := input.confirmed(MaxFlights, "", &cancellations, localDepartures)
		if len(extra.Legs) == 0 {
			continue
		}
		delete(cancelledAlternatives, parts.calendars[i].alternative)
		positiveParts++
		confirmed.Legs = append(confirmed.Legs, extra.Legs...)
		confirmed.Truncated = confirmed.Truncated || extra.Truncated
		confirmed.BookingCode = extra.BookingCode
	}
	plain := joinedEmailText(parts.plain, cancelledAlternatives)
	html := joinedEmailText(parts.html, cancelledAlternatives)
	if len(cancelledAlternatives) != 0 && plain == "" && html == "" && len(confirmed.Legs) == 0 {
		subject = ""
	}
	result := fromText(subject+"\n"+plain, "")
	before := len(result.Legs)
	result.Legs = slices.DeleteFunc(result.Legs, cancellations.matchesText)
	removed := len(result.Legs) < before
	if len(result.Legs) == 0 {
		result = fromText(subject+"\n"+html, "")
		before = len(result.Legs)
		result.Legs = slices.DeleteFunc(result.Legs, cancellations.matchesText)
		removed = removed || len(result.Legs) < before
	}
	if removed || cancellations.recognized && len(result.Legs) == 0 {
		result.BookingCode = ""
	}
	seen := make(map[Leg]bool)
	for _, leg := range result.Legs {
		seen[leg] = true
	}
	for _, leg := range confirmed.Legs {
		if !seen[leg] {
			result.Legs = append(result.Legs, leg)
			seen[leg] = true
		}
	}
	result.Legs = slices.DeleteFunc(result.Legs, func(leg Leg) bool { return localDepartures[leg] })
	result.Legs = preferPreciseLegs(result.Legs)
	if result.BookingCode == "" && len(result.Legs) == 1 && positiveParts == 1 {
		result.BookingCode = confirmed.BookingCode
	}
	result.Truncated = result.Truncated || confirmed.Truncated
	result.CancellationOnly = cancellations.recognized && len(result.Legs) == 0
	return limitItinerary(result, MaxFlights), nil
}

func joinedEmailText(parts []emailTextPart, cancelledAlternatives map[int]bool) string {
	var text strings.Builder
	for _, part := range parts {
		// Alternative parts are representations of the same invitation.
		if !cancelledAlternatives[part.alternative] {
			text.WriteString(part.text)
			text.WriteByte('\n')
		}
	}
	return text.String()
}

func (parts *emailParts) read(header func(string) string, body io.Reader, depth, alternative int) {
	if depth > 5 || parts.count >= 48 {
		return
	}
	parts.count++
	kind, params, err := mime.ParseMediaType(header("Content-Type"))
	if err != nil {
		if header("Content-Type") != "" {
			return
		}
		kind = "text/plain"
	}
	switch strings.ToLower(header("Content-Transfer-Encoding")) {
	case "base64":
		body = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	}
	if strings.HasPrefix(kind, "multipart/") && params["boundary"] != "" {
		if kind == "multipart/alternative" {
			alternative = parts.count
		}
		children := multipart.NewReader(body, params["boundary"])
		for parts.count < 48 {
			child, err := children.NextPart()
			if err != nil {
				break
			}
			parts.read(child.Header.Get, child, depth+1, alternative)
		}
		return
	}
	if kind == "message/rfc822" {
		if child, err := mail.ReadMessage(body); err == nil {
			parts.read(child.Header.Get, child.Body, depth+1, 0)
		}
		return
	}
	if kind != "text/calendar" && kind != "text/plain" && kind != "text/html" {
		return
	}
	if disposition, _, _ := mime.ParseMediaType(header("Content-Disposition")); disposition == "attachment" && kind != "text/calendar" {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(body, MaxEmailBytes+1))
	if err != nil || len(raw) > MaxEmailBytes {
		return
	}
	var value string
	switch strings.ToLower(params["charset"]) {
	case "iso-8859-1", "latin1", "windows-1252":
		var decoded strings.Builder
		for _, b := range raw {
			decoded.WriteRune(rune(b))
		}
		value = decoded.String()
	case "", "utf-8", "us-ascii":
		if !utf8.Valid(raw) {
			return
		}
		value = string(raw)
	default:
		return
	}
	switch kind {
	case "text/plain":
		parts.plain = append(parts.plain, emailTextPart{text: value, alternative: alternative})
	case "text/html":
		parts.html = append(parts.html, emailTextPart{text: htmlText(value), alternative: alternative})
	case "text/calendar":
		parts.calendars = append(parts.calendars, emailCalendarPart{
			emailTextPart: emailTextPart{text: value, alternative: alternative},
			forceCancel:   strings.EqualFold(strings.TrimSpace(params["method"]), "CANCEL"),
		})
	}
}

func preferPreciseLegs(legs []Leg) []Leg {
	precise := make(map[Leg]bool)
	for _, leg := range legs {
		if !leg.UTCDeparture.IsZero() || leg.LocalDeparture != "" {
			precise[Leg{Number: leg.Number, Date: leg.Date}] = true
		}
	}
	return slices.DeleteFunc(legs, func(leg Leg) bool { return precise[leg] })
}
