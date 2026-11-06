package itinerary

import (
	"bufio"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crizzy9/plano/internal/feeds"
)

const maxPassportCSVRows = 1000

var (
	// ErrPassportCSV identifies an unreadable or unsupported history export.
	ErrPassportCSV         = errors.New("invalid Passport CSV")
	passportAirportPattern = regexp.MustCompile(`^(?:[A-Z0-9]{3}|[A-Z]{4})$`)
	passportTailPattern    = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{0,15}$`)
	passportFlightyID      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type PassportCSVResult struct {
	Flights []feeds.Flight
	Skipped int
}

// PassportCSV reads past completed flight records without retaining the export
// or its private booking, seat, and note fields. The caller bounds the input size.
func PassportCSV(input io.Reader, now time.Time) (PassportCSVResult, error) {
	buffered := bufio.NewReader(input)
	if prefix, _ := buffered.Peek(3); string(prefix) == "\ufeff" {
		_, _ = buffered.Discard(3)
	}
	reader := csv.NewReader(buffered)
	reader.ReuseRecord = true
	header, err := reader.Read()
	if err != nil {
		return PassportCSVResult{}, ErrPassportCSV
	}
	columns, ok := passportCSVColumns(header)
	if !ok {
		return PassportCSVResult{}, ErrPassportCSV
	}
	width := len(header)
	reader.FieldsPerRecord = -1
	result := PassportCSVResult{}
	today := now.UTC().Format(time.DateOnly)
	for row := 0; ; row++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil || row == maxPassportCSVRows {
			return PassportCSVResult{}, ErrPassportCSV
		}
		if len(record) != width {
			result.Skipped++
			continue
		}
		flight, ok := passportCSVFlight(func(name string) string {
			if index, exists := columns[name]; exists {
				return strings.TrimSpace(record[index])
			}
			return ""
		}, now, today)
		if !ok {
			result.Skipped++
			continue
		}
		result.Flights = append(result.Flights, flight)
	}
}

func passportCSVColumns(header []string) (map[string]int, bool) {
	columns := make(map[string]int)
	for index, label := range header {
		name := strings.ToLower(strings.TrimSpace(label))
		switch name {
		case "date", "airline", "flight", "from", "to", "canceled", "cancelled", "diverted to",
			"gate departure (scheduled)", "gate departure (actual)", "gate arrival (scheduled)", "gate arrival (actual)",
			"aircraft type name", "tail number", "flight flighty id":
			if _, duplicate := columns[name]; duplicate {
				return nil, false
			}
			columns[name] = index
		}
	}
	_, from := columns["from"]
	_, to := columns["to"]
	_, date := columns["date"]
	_, scheduled := columns["gate departure (scheduled)"]
	_, actual := columns["gate departure (actual)"]
	return columns, from && to && (date || scheduled || actual)
}

func passportCSVFlight(field func(string) string, now time.Time, today string) (feeds.Flight, bool) {
	for _, name := range []string{"canceled", "cancelled"} {
		switch strings.ToLower(field(name)) {
		case "", "0", "false", "f", "no":
		default:
			return feeds.Flight{}, false
		}
	}
	if field("diverted to") != "" {
		return feeds.Flight{}, false
	}
	from, to := strings.ToUpper(field("from")), strings.ToUpper(field("to"))
	if !passportAirportPattern.MatchString(from) || !passportAirportPattern.MatchString(to) || from == to {
		return feeds.Flight{}, false
	}
	scheduledOut, scheduledOutAbsolute := passportCSVTimestamp(field("gate departure (scheduled)"))
	actualOut, actualOutAbsolute := passportCSVTimestamp(field("gate departure (actual)"))
	date := field("date")
	if date == "" {
		switch {
		case !scheduledOut.IsZero():
			date = scheduledOut.Format(time.DateOnly)
		case !actualOut.IsZero():
			date = actualOut.Format(time.DateOnly)
		}
	}
	day, err := time.Parse(time.DateOnly, date)
	if err != nil || day.Year() < 1900 || day.Format(time.DateOnly) != date || date >= today {
		return feeds.Flight{}, false
	}
	flight := feeds.Flight{
		Number: passportCSVFlightNumber(field("airline"), field("flight")), Origin: from, Destination: to,
		HistoryDate: date, Status: "arrived", Source: "flighty_csv", ObservedAt: now.UTC(),
	}
	if len(from) == 4 {
		flight.OriginICAO = from
	}
	if len(to) == 4 {
		flight.DestinationICAO = to
	}
	if tail := strings.ToUpper(field("tail number")); passportTailPattern.MatchString(tail) {
		flight.TailNumber = tail
	}
	if model := field("aircraft type name"); len(model) <= 80 && utf8.ValidString(model) && !strings.ContainsFunc(model, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) {
		flight.AircraftModel = model
	}
	firstDate, lastDate := day.AddDate(0, 0, -1).Format(time.DateOnly), day.AddDate(0, 0, 2).Format(time.DateOnly)
	nearDate := func(at time.Time) bool {
		localDate := at.Format(time.DateOnly)
		return !at.IsZero() && localDate >= firstDate && localDate <= lastDate
	}
	if scheduledOutAbsolute && nearDate(scheduledOut) {
		flight.ScheduledDeparture = new(scheduledOut.UTC())
	}
	if actualOutAbsolute && nearDate(actualOut) && !actualOut.After(now) {
		flight.ActualDeparture = new(actualOut.UTC())
	}
	if at, absolute := passportCSVTimestamp(field("gate arrival (scheduled)")); absolute && nearDate(at) && passportCSVArrivalFollowsDeparture(flight.ScheduledDeparture, at) {
		flight.ScheduledArrival = new(at.UTC())
	}
	if at, absolute := passportCSVTimestamp(field("gate arrival (actual)")); absolute && nearDate(at) && !at.After(now) && passportCSVArrivalFollowsDeparture(flight.ActualDeparture, at) {
		flight.ActualArrival = new(at.UTC())
	}
	identity := strings.ToLower(field("flight flighty id"))
	if passportFlightyID.MatchString(identity) {
		identity = "flighty-id\x00" + identity
	} else {
		var clock string
		switch {
		case nearDate(scheduledOut):
			clock = passportCSVClockKey(scheduledOut, scheduledOutAbsolute)
		case nearDate(actualOut):
			clock = passportCSVClockKey(actualOut, actualOutAbsolute)
		}
		identity = strings.Join([]string{"flighty-flight", date, flight.Number, from, to, clock}, "\x00")
	}
	flight.ID = fmt.Sprintf("archive-%x", sha256.Sum256([]byte(identity)))
	return flight, true
}

func passportCSVFlightNumber(airline, flight string) string {
	if flight == "" {
		return ""
	}
	for _, candidate := range []string{flight, airline + flight} {
		number := strings.ToUpper(candidate)
		if match := flightPattern.FindStringSubmatch(number); match != nil && match[0] == number {
			return match[1] + match[2]
		}
	}
	return ""
}

// Offset-free times can identify records and supply a calendar date. They
// cannot establish an absolute time without the airport's historical zone.
func passportCSVTimestamp(value string) (time.Time, bool) {
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		_, offset := at.Zone()
		if offset >= -14*60*60 && offset <= 14*60*60 {
			return at, true
		}
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if at, err := time.Parse(layout, value); err == nil {
			return at, false
		}
	}
	return time.Time{}, false
}

func passportCSVClockKey(at time.Time, absolute bool) string {
	if absolute {
		return at.UTC().Format(time.RFC3339Nano)
	}
	return at.Format("2006-01-02T15:04:05.999999999")
}

func passportCSVArrivalFollowsDeparture(departure *time.Time, arrival time.Time) bool {
	return departure == nil || arrival.After(*departure) && arrival.Sub(*departure) <= 48*time.Hour
}
