package itinerary

import (
	"encoding/xml"
	"strings"
)

const (
	maxEmailTableDepth   = 16
	maxEmailTableColumns = 64
)

type emailHTMLSpan struct{ start, end int }

type emailHTMLEdit struct {
	emailHTMLSpan
	text string
}

type emailHTMLRow struct {
	start, firstEdit, depth int
	cells                   []emailHTMLSpan
	cell                    string
	cellStart, headerCells  int
	complex, rowSpan        bool
}

type emailHTMLColumns struct{ count, flight, departure, departureTime, arrival int }

type emailHTMLTable struct {
	columns    emailHTMLColumns
	classified bool
	inHead     bool
	rowSpan    bool
	row        *emailHTMLRow
}

func htmlText(value string) string {
	decoder := xml.NewDecoder(strings.NewReader(value))
	decoder.Strict, decoder.AutoClose, decoder.Entity = false, xml.HTMLAutoClose, xml.HTMLEntity
	var output strings.Builder
	var tables [maxEmailTableDepth]emailHTMLTable
	var edits []emailHTMLEdit
	var hidden, tableDepth int
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		var table *emailHTMLTable
		if tableDepth > 0 && tableDepth <= len(tables) {
			table = &tables[tableDepth-1]
		}
		switch current := token.(type) {
		case xml.StartElement:
			name := strings.ToLower(current.Name.Local)
			if hiddenEmailElement(name) {
				hidden++
			}
			if hidden != 0 {
				continue
			}
			switch name {
			case "br", "p", "div", "tr":
				if name == "tr" && table != nil {
					if table.row == nil {
						table.row = &emailHTMLRow{start: output.Len(), firstEdit: len(edits), depth: 1}
					} else {
						table.row.depth++
						table.row.complex = true
					}
				}
				output.WriteByte('\n')
			case "td", "th":
				output.WriteByte(' ')
				if table != nil && table.row != nil {
					table.row.openCell(name, output.Len(), current.Attr)
				}
			case "table":
				if table != nil && table.row != nil {
					table.row.complex = true
				}
				tableDepth++
				if tableDepth <= len(tables) {
					tables[tableDepth-1] = emailHTMLTable{}
				}
			case "thead":
				if table != nil {
					table.inHead = true
				}
			}
		case xml.EndElement:
			name := strings.ToLower(current.Name.Local)
			if hidden != 0 {
				if hiddenEmailElement(name) {
					hidden--
				}
				continue
			}
			if name == "table" && tableDepth > 0 {
				if table != nil && table.row != nil {
					table.row.complex = true
					edits = table.finishRow(output.String(), edits)
				}
				tableDepth--
			} else if table != nil {
				switch name {
				case "thead":
					table.inHead = false
				case "td", "th":
					if table.row != nil {
						table.row.closeCell(name, output.Len())
					}
				case "tr":
					if table.row != nil {
						table.row.depth--
						if table.row.depth == 0 {
							edits = table.finishRow(output.String(), edits)
						}
					}
				}
			}
		case xml.CharData:
			if hidden == 0 {
				output.Write(current)
			}
		}
	}
	plain := output.String()
	// A truncated row in a classified flight table cannot establish which
	// column supplied its date. Retire it before applying any inner edits.
	for i := min(tableDepth, len(tables)) - 1; i >= 0; i-- {
		if table := &tables[i]; table.row != nil && table.classified {
			table.row.complex = true
			edits = table.finishRow(plain, edits)
		}
	}
	if len(edits) == 0 {
		return plain
	}
	var flattened strings.Builder
	var start int
	for _, edit := range edits {
		flattened.WriteString(plain[start:edit.start])
		flattened.WriteString(edit.text)
		start = edit.end
	}
	flattened.WriteString(plain[start:])
	return flattened.String()
}

func hiddenEmailElement(name string) bool {
	return name == "head" || name == "script" || name == "style"
}

func (r *emailHTMLRow) openCell(name string, start int, attrs []xml.Attr) {
	if r.cell != "" || r.depth != 1 || len(r.cells) >= maxEmailTableColumns {
		r.complex = true
		return
	}
	r.cell, r.cellStart = name, start
	if name == "th" {
		r.headerCells++
	}
	for _, attr := range attrs {
		if strings.TrimSpace(attr.Value) == "1" {
			continue
		}
		switch strings.ToLower(attr.Name.Local) {
		case "rowspan":
			r.rowSpan = true
		case "colspan":
			r.complex = true
		}
	}
}

func (r *emailHTMLRow) closeCell(name string, end int) {
	if r.cell != name || r.depth != 1 {
		r.complex = true
		return
	}
	r.cells = append(r.cells, emailHTMLSpan{r.cellStart, end})
	r.cell = ""
}

func (t *emailHTMLTable) finishRow(output string, edits []emailHTMLEdit) []emailHTMLEdit {
	row := t.row
	t.row = nil
	row.complex = row.complex || row.cell != ""
	// A vertical span can shift columns in later rows as well as this one.
	t.rowSpan = t.rowSpan || row.rowSpan
	rowIsHeader := t.inHead
	if t.inHead || row.headerCells != 0 {
		columns, classified := readEmailHTMLColumns(output, row.cells)
		rowIsHeader = t.inHead || classified
		if rowIsHeader {
			t.classified = t.classified || classified
			t.columns = emailHTMLColumns{}
			if !row.complex {
				t.columns = columns
			}
		}
	}
	if !t.classified {
		return edits
	}
	// Once a header identifies a flight table, use only the recognized fields.
	// A shifted, empty, or ambiguous departure cannot fall through to a date
	// taken from another column. Keep independent booking labels in either case.
	var replacement strings.Builder
	replacement.WriteByte('\n')
	if !rowIsHeader && !row.complex && !t.rowSpan && t.columns.count != 0 && len(row.cells) == t.columns.count {
		replacement.WriteString(t.columns.flightRow(output, row.cells))
	}
	for _, locator := range booking.FindAllString(output[row.start:], -1) {
		replacement.WriteString(locator)
		replacement.WriteByte('\n')
	}
	return append(edits[:row.firstEdit], emailHTMLEdit{emailHTMLSpan{row.start, len(output)}, replacement.String()})
}

func readEmailHTMLColumns(output string, cells []emailHTMLSpan) (emailHTMLColumns, bool) {
	columns := emailHTMLColumns{count: len(cells), flight: -1, departure: -1, departureTime: -1, arrival: -1}
	var duplicate bool
	for i, cell := range cells {
		var index *int
		switch strings.ToLower(cell.value(output)) {
		case "flight", "flight number", "flight no", "flight no.":
			index = &columns.flight
		case "departure", "departs", "departure date", "departure date/time":
			index = &columns.departure
		case "departure time":
			index = &columns.departureTime
		case "arrival", "arrives", "arrival date", "arrival date/time":
			index = &columns.arrival
		}
		if index != nil {
			duplicate = duplicate || *index >= 0
			*index = i
		}
	}
	classified := columns.flight >= 0 && (columns.departure >= 0 || columns.departureTime >= 0 || columns.arrival >= 0)
	if duplicate || columns.flight < 0 || columns.departure < 0 {
		return emailHTMLColumns{}, classified
	}
	return columns, classified
}

func (c emailHTMLColumns) flightRow(output string, cells []emailHTMLSpan) string {
	number := strings.TrimPrefix(strings.ToUpper(cells[c.flight].value(output)), "FLIGHT ")
	departure := cells[c.departure].value(output)
	match := flightPattern.FindStringIndex(number)
	if match == nil || match[0] != 0 || match[1] != len(number) || departure == "" || ambiguousEmailScheduleCell(departure) {
		return ""
	}
	line := "Flight " + number + " | Departure: " + departure
	if c.departureTime >= 0 {
		value := cells[c.departureTime].value(output)
		if value != "" {
			if clock, _ := departureClock(value); clock == "" {
				return ""
			}
			line += " | Departure time: " + value
		}
	}
	if c.arrival >= 0 {
		arrival := cells[c.arrival].value(output)
		if ambiguousEmailScheduleCell(arrival) {
			return ""
		}
		if arrival != "" {
			line += " | Arrival: " + arrival
		}
	}
	return line + "\n"
}

func ambiguousEmailScheduleCell(value string) bool {
	return strings.Contains(value, "|") || scheduleLabel.MatchString(value)
}

func (s emailHTMLSpan) value(output string) string {
	return strings.Join(strings.Fields(output[s.start:s.end]), " ")
}
