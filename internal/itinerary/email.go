package itinerary

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"unicode/utf8"
)

const MaxEmailBytes = 256 << 10

type emailParts struct {
	plain, html, calendars []string
	count                  int
}

// Email extracts itinerary facts from RFC 5322 mail. Attachments other than
// calendar invitations are ignored and links are never fetched.
func Email(raw []byte) (Itinerary, error) {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Itinerary{}, ErrInvalid
	}
	var parts emailParts
	parts.read(message.Header.Get, message.Body, 0)
	decoder := mime.WordDecoder{}
	subject, err := decoder.DecodeHeader(message.Header.Get("Subject"))
	if err != nil {
		subject = ""
	}
	plain := strings.Join(parts.plain, "\n")
	result := fromText(subject+"\n"+plain, "")
	if len(result.Legs) == 0 {
		result = fromText(subject+"\n"+strings.Join(parts.html, "\n"), "")
	}
	seen := make(map[Leg]bool)
	for _, leg := range result.Legs {
		seen[leg] = true
	}
	for _, value := range parts.calendars {
		extra, err := calendar(value, MaxFlights)
		if err != nil {
			continue
		}
		result.Truncated = result.Truncated || extra.Truncated
		for _, leg := range extra.Legs {
			if !seen[leg] {
				result.Legs = append(result.Legs, leg)
				seen[leg] = true
			}
		}
		if result.BookingCode == "" && len(parts.calendars) == 1 && len(result.Legs) == 1 {
			result.BookingCode = extra.BookingCode
		}
	}
	return limitItinerary(result, MaxFlights), nil
}

func (parts *emailParts) read(header func(string) string, body io.Reader, depth int) {
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
		children := multipart.NewReader(body, params["boundary"])
		for parts.count < 48 {
			child, err := children.NextPart()
			if err != nil {
				break
			}
			parts.read(child.Header.Get, child, depth+1)
		}
		return
	}
	if kind == "message/rfc822" {
		if child, err := mail.ReadMessage(body); err == nil {
			parts.read(child.Header.Get, child.Body, depth+1)
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
		parts.plain = append(parts.plain, value)
	case "text/html":
		parts.html = append(parts.html, htmlText(value))
	case "text/calendar":
		parts.calendars = append(parts.calendars, value)
	}
}

func htmlText(value string) string {
	decoder := xml.NewDecoder(strings.NewReader(value))
	decoder.Strict, decoder.AutoClose, decoder.Entity = false, xml.HTMLAutoClose, xml.HTMLEntity
	var output strings.Builder
	var hidden int
	for {
		token, err := decoder.Token()
		if err != nil {
			return output.String()
		}
		switch current := token.(type) {
		case xml.StartElement:
			name := strings.ToLower(current.Name.Local)
			if name == "script" || name == "style" || name == "head" {
				hidden++
			}
			if hidden == 0 && (name == "br" || name == "p" || name == "div" || name == "tr") {
				output.WriteByte('\n')
			}
			if hidden == 0 && (name == "td" || name == "th") {
				output.WriteByte(' ')
			}
		case xml.EndElement:
			name := strings.ToLower(current.Name.Local)
			if hidden > 0 && (name == "script" || name == "style" || name == "head") {
				hidden--
			}
		case xml.CharData:
			if hidden == 0 {
				output.Write(current)
			}
		}
	}
}
