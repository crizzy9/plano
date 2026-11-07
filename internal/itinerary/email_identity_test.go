package itinerary

import (
	"strings"
	"testing"
)

func TestEmailWithMessageID(t *testing.T) {
	const originalID = "<Booking+Reminder.1@EXAMPLE.COM>"
	const message = "Subject: Itinerary\r\nContent-Type: text/plain\r\n\r\nUA123 | Departure: 5 January 2027\r\n"
	for _, test := range []struct {
		name, headers, want string
	}{
		{"original", "Message-ID: " + originalID + "\r\n", originalID},
		{"forwarded trace", "Received: by relay.example\r\nResent-Message-ID: <forward@relay.example>\r\n" +
			"message-id:\r\n\t" + originalID + "\r\n", originalID},
		{"domain literal", "Message-ID: <booking@[IPv6:2001:db8::1]>\r\n", "<booking@[IPv6:2001:db8::1]>"},
		{"absent", "Resent-Message-ID: <forward@relay.example>\r\n", ""},
		{"duplicate header", "Message-ID: " + originalID + "\r\nmessage-id: " + originalID + "\r\n", ""},
		{"multiple IDs", "Message-ID: " + originalID + " <other@example.com>\r\n", ""},
		{"bare", "Message-ID: booking@example.com\r\n", ""},
		{"empty atom", "Message-ID: <booking..reminder@example.com>\r\n", ""},
		{"empty domain", "Message-ID: <booking@>\r\n", ""},
		{"embedded whitespace", "Message-ID: <booking@exa\tmple.com>\r\n", ""},
		{"embedded control", "Message-ID: <booking\x01@example.com>\r\n", ""},
		{"oversized", "Message-ID: <" + strings.Repeat("a", maxEmailMessageIDBytes) + "@example.com>\r\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			itinerary, id, err := EmailWithMessageID([]byte(test.headers + message))
			if err != nil || id != test.want {
				t.Fatalf("got Message-ID %q, err %v, want %q", id, err, test.want)
			}
			if len(itinerary.Legs) != 1 || itinerary.Legs[0].Number != "UA123" || itinerary.Legs[0].Date != "2027-01-05" {
				t.Fatalf("unexpected itinerary: %+v", itinerary)
			}
		})
	}
}

func TestEmailWithMessageIDIgnoresAttachedMessageID(t *testing.T) {
	const message = "Content-Type: message/rfc822\r\n\r\n" +
		"Message-ID: <attached@example.com>\r\nSubject: Itinerary\r\n\r\nUA123 | Departure: 5 January 2027\r\n"
	itinerary, id, err := EmailWithMessageID([]byte(message))
	if err != nil || id != "" || len(itinerary.Legs) != 1 || itinerary.Legs[0].Number != "UA123" {
		t.Fatalf("got itinerary %+v, Message-ID %q, err %v", itinerary, id, err)
	}
}
