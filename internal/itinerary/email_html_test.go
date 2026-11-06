package itinerary

import (
	"strings"
	"testing"
)

func TestHTMLTextFlightTable(t *testing.T) {
	tests := []struct {
		name string
		html string
		want []string
		omit []string
	}{
		{
			name: "overnight",
			html: `<table><tr><th>Flight</th><th>Departure</th><th>Arrival</th></tr>
<tr><td>UA&nbsp;123</td><td>7 December 2026<br>22:00</td><td>8 December 2026 06:00</td></tr></table>`,
			want: []string{"\nFlight UA 123 | Departure: 7 December 2026 22:00 | Arrival: 8 December 2026 06:00\n"},
		},
		{
			name: "reordered columns",
			html: `<table><thead><tr><td>Arrival date</td><td>Flight number</td><td>Departure date</td></tr></thead><tbody>
<tr><td>8 December 2026</td><td><strong>UA123</strong></td><td>7 December 2026</td></tr>
<tr><td></td><td>UA124</td><td>9 December 2026</td></tr></tbody></table>`,
			want: []string{
				"\nFlight UA123 | Departure: 7 December 2026 | Arrival: 8 December 2026\n",
				"\nFlight UA124 | Departure: 9 December 2026\n",
			},
		},
		{
			name: "separate departure date and time",
			html: `<table><tr><th>Flight</th><th>Departure Date</th><th>Departure Time</th><th>Arrival Date</th></tr>
<tr><td>UA123</td><td>7 December 2026</td><td>10:00 PM</td><td>8 December 2026</td></tr>
<tr><td>UA456</td><td>7 December 2026</td><td>TBD</td><td>8 December 2026</td></tr></table>`,
			want: []string{"\nFlight UA123 | Departure: 7 December 2026 | Departure time: 10:00 PM | Arrival: 8 December 2026\n"},
			omit: []string{"UA456"},
		},
		{
			name: "existing unlabeled row and hidden elements",
			html: `<head><title>UA111</title></head><script>UA222</script><style>UA333</style>
<table><tr><td>UA123</td><td>7 December 2026</td></tr></table>`,
			want: []string{"\n UA123 7 December 2026"},
			omit: []string{"UA111", "UA222", "UA333", "Departure:"},
		},
		{
			name: "nested table and independent layout table",
			html: `<table><tr><td>Summary<table><tr><th>Departure</th><th>Flight</th></tr>
<tr><td>7 December 2026</td><td>UA123</td></tr></table></td></tr></table>
<table><tr><td>AA456</td><td>8 December 2026</td></tr></table>`,
			want: []string{" Summary", "\nFlight UA123 | Departure: 7 December 2026\n", "\n AA456 8 December 2026"},
			omit: []string{"Flight AA456 |"},
		},
		{
			name: "ambiguous and shifted columns",
			html: `<table><tr><th>Flight</th><th>Departure</th><th>Departure date</th></tr>
<tr><td>UA111</td><td>7 December 2026</td><td>8 December 2026</td></tr></table>
<table><tr><th>Flight</th><th>Departure</th><th>Arrival</th></tr>
<tr><td>UA222</td><td colspan="2">8 December 2026</td></tr>
<tr><td>UA333</td><td></td><td>8 December 2026</td></tr>
<tr><td>UA444</td><td>8 December 2026</td></tr>
<tr><td>UA555</td><td>7 December 2026</td><td>8 December 2026</td></tr>
<tr><td>UA666</td><td>06:00</td><td>Departure: 8 December 2026</td></tr>
<tr><th>UA999</th><th></th><th>8 December 2026</th></tr>
<tr><td>UA777</td><td rowspan="2">8 December 2026</td><td>8 December 2026</td></tr>
<tr><td>UA888</td><td>9 December 2026</td><td>9 December 2026</td></tr></table>`,
			want: []string{"\nFlight UA555 | Departure: 7 December 2026 | Arrival: 8 December 2026\n"},
			omit: []string{"UA111", "UA222", "UA333", "UA444", "UA666", "UA777", "UA888", "UA999"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := htmlText(tt.html)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("htmlText() = %q, want it to contain %q", got, want)
				}
			}
			for _, omit := range tt.omit {
				if strings.Contains(got, omit) {
					t.Errorf("htmlText() = %q, want it to omit %q", got, omit)
				}
			}
		})
	}
}
