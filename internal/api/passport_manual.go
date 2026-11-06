package api

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

var (
	manualPassportAirport = regexp.MustCompile(`^(?:[A-Z0-9]{3}|[A-Z]{4})$`)
	manualPassportTail    = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{0,15}$`)
)

type manualPassportInput struct {
	Date                string     `json:"date"`
	Origin              string     `json:"origin"`
	Destination         string     `json:"destination"`
	Number              string     `json:"number"`
	TailNumber          string     `json:"tailNumber"`
	AircraftModel       string     `json:"aircraftModel"`
	DurationMinutes     *int       `json:"durationMinutes"`
	ArrivalDelayMinutes *int       `json:"arrivalDelayMinutes"`
	RequestID           string     `json:"requestId"`
	ExpectedObservedAt  *time.Time `json:"expectedObservedAt"`
}

func (s Server) saveManualPassport(w http.ResponseWriter, r *http.Request, owner string) error {
	var input manualPassportInput
	if err := parse(w, r, &input, 4096); err != nil {
		return err
	}
	flight, err := input.flight(time.Now().UTC())
	if err != nil {
		return err
	}
	if r.Method == http.MethodPut {
		flight.ID = r.PathValue("id")
		if input.RequestID != "" || !passportArchiveID.MatchString(flight.ID) ||
			input.ExpectedObservedAt == nil || input.ExpectedObservedAt.IsZero() {
			return errInvalid
		}
		err = s.Store.UpdateManualPassportFlight(r.Context(), owner, flight, *input.ExpectedObservedAt)
		if errors.Is(err, core.ErrPassportArchiveConflict) {
			respond(w, http.StatusConflict, map[string]string{"error": "The flight changed. Reload Passport before editing it again."})
			return nil
		}
		if err == nil {
			w.WriteHeader(http.StatusNoContent)
		}
		return err
	}
	requestID := strings.ToLower(input.RequestID)
	if !uuidV4.MatchString(requestID) || input.ExpectedObservedAt != nil {
		return errInvalid
	}
	flight.ID = fmt.Sprintf("archive-%x", sha256.Sum256([]byte("passport-manual\x00"+requestID)))
	created, err := s.Store.CreateManualPassportFlight(r.Context(), owner, flight)
	if errors.Is(err, core.ErrPassportArchiveFull) {
		respond(w, http.StatusConflict, map[string]string{"error": "Passport can store up to 10,000 archived flights"})
		return nil
	}
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	respond(w, status, map[string]bool{"created": created})
	return nil
}

func (input manualPassportInput) flight(now time.Time) (feeds.Flight, error) {
	date := strings.TrimSpace(input.Date)
	day, err := time.Parse(time.DateOnly, date)
	if err != nil || day.Year() < 1900 || date >= now.Format(time.DateOnly) {
		return feeds.Flight{}, errInvalid
	}
	from := strings.ToUpper(strings.TrimSpace(input.Origin))
	to := strings.ToUpper(strings.TrimSpace(input.Destination))
	number := strings.ToUpper(strings.Join(strings.Fields(input.Number), ""))
	tail := strings.ToUpper(strings.TrimSpace(input.TailNumber))
	model := strings.TrimSpace(input.AircraftModel)
	if !manualPassportAirport.MatchString(from) || !manualPassportAirport.MatchString(to) || from == to ||
		number != "" && !flightNumber.MatchString(number) || tail != "" && !manualPassportTail.MatchString(tail) ||
		utf8.RuneCountInString(model) > 80 || strings.ContainsFunc(model, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
	}) || !validManualPassportMinutes(input.DurationMinutes, 1) || !validManualPassportMinutes(input.ArrivalDelayMinutes, 0) {
		return feeds.Flight{}, errInvalid
	}
	flight := feeds.Flight{Number: number, Origin: from, Destination: to, HistoryDate: date,
		Status: "arrived", Source: feeds.PassportManualSource, ObservedAt: now, TailNumber: tail, AircraftModel: model,
		HistoryDurationMinutes: input.DurationMinutes, HistoryArrivalDelayMinutes: input.ArrivalDelayMinutes}
	if len(from) == 4 {
		flight.OriginICAO = from
	}
	if len(to) == 4 {
		flight.DestinationICAO = to
	}
	return flight, nil
}

func validManualPassportMinutes(value *int, minimum int) bool {
	return value == nil || *value >= minimum && *value <= feeds.PassportMaxHistoryMinutes
}
