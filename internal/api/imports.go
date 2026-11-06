package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/itinerary"
)

type importResult struct {
	Flights    []feeds.Flight `json:"flights"`
	Unmatched  []string       `json:"unmatched"`
	Truncated  bool           `json:"truncated,omitempty"`
	NextOffset *int           `json:"nextOffset,omitempty"`
}

func (s Server) importFlights(w http.ResponseWriter, r *http.Request, owner string) error {
	var input struct{ Source, Text, Date string }
	if err := parse(w, r, &input, 64<<10); err != nil {
		return err
	}
	if input.Source != "calendar" && input.Source != "email" && input.Source != "tripit" || strings.TrimSpace(input.Text) == "" {
		return errInvalid
	}
	parsed, err := itinerary.Parse(input.Text, input.Date)
	if err != nil || len(parsed.Legs) == 0 {
		return errInvalid
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	result, err := s.resolveItinerary(ctx, parsed)
	if err != nil {
		return err
	}
	if _, err := s.Store.CommitImport(ctx, owner, result.Flights, s.bookingDigest(owner, parsed.BookingCode), "", ""); err != nil {
		return err
	}
	respond(w, 200, result)
	return nil
}

func (s Server) resolveItinerary(ctx context.Context, parsed itinerary.Itinerary) (importResult, error) {
	result := importResult{Flights: []feeds.Flight{}, Unmatched: []string{}, Truncated: parsed.Truncated}
	seen := make(map[string]bool)
	for _, leg := range parsed.Legs {
		matches, err := s.lookupLeg(ctx, leg)
		if err != nil {
			return result, err
		}
		if len(matches) != 1 {
			result.Unmatched = append(result.Unmatched, leg.Number+" · "+leg.Date)
			continue
		}
		if !seen[matches[0].ID] {
			result.Flights = append(result.Flights, matches[0])
			seen[matches[0].ID] = true
		}
	}
	return result, nil
}

func (s Server) lookupLeg(ctx context.Context, leg itinerary.Leg) ([]feeds.Flight, error) {
	matchesRoute := func(flight feeds.Flight) bool {
		return importAirportMatches(leg.Origin, flight.Origin, flight.OriginICAO) &&
			importAirportMatches(leg.Destination, flight.Destination, flight.DestinationICAO)
	}
	if leg.UTCDeparture.IsZero() {
		flights, err := s.lookup(ctx, leg.Number, leg.Date)
		if err != nil || leg.LocalDeparture == "" && leg.Origin == "" && leg.Destination == "" {
			return flights, err
		}
		seen := make(map[string]bool)
		var matches []feeds.Flight
		for _, flight := range flights {
			if seen[flight.ID] || !matchesRoute(flight) {
				continue
			}
			if leg.LocalDeparture != "" && (flight.ScheduledDeparture == nil ||
				!leg.MatchesLocalDeparture(*flight.ScheduledDeparture, flight.OriginTimeZone)) {
				continue
			}
			seen[flight.ID] = true
			matches = append(matches, flight)
		}
		return matches, nil
	}
	utc := leg.UTCDeparture.UTC()
	dates := []string{utc.Format(time.DateOnly), utc.AddDate(0, 0, -1).Format(time.DateOnly), utc.AddDate(0, 0, 1).Format(time.DateOnly)}
	primary := leg.Date
	if !slices.Contains(dates, primary) {
		// An airport-local date can differ from the UTC date by at most one day.
		primary = dates[0]
	}
	var exact, nearby []feeds.Flight
	seenExact, seenNearby := make(map[string]bool), make(map[string]bool)
	addMatches := func(flights []feeds.Flight) {
		for _, f := range flights {
			if f.ScheduledDeparture == nil || !matchesRoute(f) {
				continue
			}
			difference := f.ScheduledDeparture.Sub(utc).Abs()
			switch {
			case difference == 0 && !seenExact[f.ID]:
				seenExact[f.ID] = true
				exact = append(exact, f)
			case difference > 0 && difference <= 45*time.Minute && !seenNearby[f.ID]:
				seenNearby[f.ID] = true
				nearby = append(nearby, f)
			}
		}
	}
	flights, err := s.lookup(ctx, leg.Number, primary)
	if err != nil {
		return nil, err
	}
	addMatches(flights)
	if len(exact) != 0 {
		return exact, nil
	}
	// A nearby flight on the source date can mask an exact match on an adjacent date.
	for _, date := range dates {
		if date == primary {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		flights, err := s.lookup(ctx, leg.Number, date)
		if err != nil {
			return nil, err
		}
		addMatches(flights)
	}
	if len(exact) != 0 {
		return exact, nil
	}
	return nearby, nil
}

func importAirportMatches(expected, reported, icao string) bool {
	if expected == "" {
		return true
	}
	if len(expected) == 4 && icao != "" {
		reported = icao
	}
	return strings.EqualFold(expected, reported)
}

func (s Server) emailRelay(w http.ResponseWriter, r *http.Request, owner string) error {
	if r.Method == http.MethodDelete {
		if err := s.Store.RemoveEmailRelay(r.Context(), owner); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	token, err := s.Store.RotateEmailRelay(r.Context(), owner)
	if err != nil {
		return err
	}
	respond(w, http.StatusCreated, map[string]string{"url": s.PublicURL + "/v1/webhooks/email", "token": token})
	return nil
}

func (s Server) emailWebhook(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(token) < 20 || len(token) > 128 {
		respond(w, 401, map[string]string{"error": "valid email relay token required"})
		return
	}
	owner, err := s.Store.EmailRelayOwner(r.Context(), token)
	if errors.Is(err, core.ErrNotFound) {
		respond(w, 401, map[string]string{"error": "valid email relay token required"})
		return
	}
	if err != nil {
		s.failure(w, err)
		return
	}
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "message/rfc822" {
		respond(w, 415, map[string]string{"error": "expected message/rfc822"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, itinerary.MaxEmailBytes))
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			respond(w, 413, map[string]string{"error": "email exceeds size limit"})
		} else {
			respond(w, 503, map[string]string{"error": "email could not be read"})
		}
		return
	}
	checksum := sha256.Sum256(raw)
	digest := hex.EncodeToString(checksum[:])
	seen, err := s.Store.ImportedEmail(r.Context(), owner, digest)
	if err != nil {
		s.failure(w, err)
		return
	}
	if seen {
		w.WriteHeader(204)
		return
	}
	parsed, err := itinerary.Email(raw)
	if err == nil && parsed.CancellationOnly {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil || len(parsed.Legs) == 0 {
		respond(w, 422, map[string]string{"error": "email has no recognized dated flights"})
		return
	}
	if parsed.Truncated {
		respond(w, 422, map[string]string{"error": "email exceeds the flight import limit"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	progress, err := s.Store.EmailImportProgress(ctx, owner, digest)
	if err != nil {
		s.failure(w, err)
		return
	}
	wanted := make([]string, 0, len(parsed.Legs))
	resolved := make([]core.EmailImportLeg, 0, len(parsed.Legs))
	for _, leg := range parsed.Legs {
		key := emailLegKey(digest, leg)
		if slices.Contains(wanted, key) {
			continue
		}
		wanted = append(wanted, key)
		if progress[key] {
			continue
		}
		matches, err := s.lookupLeg(ctx, leg)
		if err != nil {
			s.failure(w, err)
			return
		}
		if len(matches) == 1 {
			resolved = append(resolved, core.EmailImportLeg{Key: key, FlightID: matches[0].ID})
		}
	}
	complete, partial, err := s.Store.CommitEmailImport(ctx, owner, digest, token, s.bookingDigest(owner, parsed.BookingCode), wanted, resolved)
	if errors.Is(err, core.ErrEmailRelayUnauthorized) {
		respond(w, 401, map[string]string{"error": "valid email relay token required"})
		return
	}
	if errors.Is(err, core.ErrNotFound) {
		respond(w, 503, map[string]string{"error": "resolved flight temporarily unavailable"})
		return
	}
	if err != nil {
		s.failure(w, err)
		return
	}
	switch {
	case complete:
		w.WriteHeader(204)
	case partial:
		w.Header().Set("Retry-After", "300")
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "some flights could not yet be matched"})
	default:
		respond(w, 422, map[string]string{"error": "email has no uniquely matching flights"})
	}
}

func emailLegKey(digest string, leg itinerary.Leg) string {
	// Include the message digest so equal legs in separate emails have
	// independent receipts. Normalize time zones to the same instant.
	key := sha256.Sum256([]byte(strings.Join([]string{
		digest, leg.Number, leg.Date, leg.LocalDeparture, leg.UTCDeparture.UTC().Format(time.RFC3339Nano),
	}, "\x00")))
	return hex.EncodeToString(key[:])
}

var bookingPattern = regexp.MustCompile(`^[A-Z0-9]{6}$`)

func (s Server) resolveBooking(w http.ResponseWriter, r *http.Request, _ string) error {
	if s.ReservationSource == nil {
		return feeds.ErrUnavailable
	}
	var input struct {
		Carrier  string `json:"carrier"`
		Locator  string `json:"locator"`
		LastName string `json:"lastName"`
	}
	if err := parse(w, r, &input, 1024); err != nil {
		return err
	}
	query, err := feeds.NormalizeReservationQuery(feeds.ReservationQuery{Carrier: input.Carrier, Locator: input.Locator, Surname: input.LastName})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	legs, err := s.ReservationSource.Lookup(ctx, query)
	if errors.Is(err, feeds.ErrNotFound) || err == nil && len(legs) == 0 {
		return feeds.ErrNotFound
	}
	if err != nil || len(legs) > itinerary.MaxFlights {
		return feeds.ErrUnavailable
	}
	parsed := itinerary.Itinerary{Legs: make([]itinerary.Leg, 0, len(legs))}
	for _, leg := range legs {
		parsed.Legs = append(parsed.Legs, itinerary.Leg{
			Number: leg.Number, Date: leg.Date, Origin: leg.Origin,
			Destination: leg.Destination, UTCDeparture: leg.Departure,
		})
	}
	result, err := s.resolveItinerary(ctx, parsed)
	if err != nil {
		return err
	}
	respond(w, http.StatusOK, result)
	return nil
}

func (s Server) bookingDigest(owner, code string) string {
	if len(s.BookingKey) == 0 || !bookingPattern.MatchString(code) {
		return ""
	}
	digest := hmac.New(sha256.New, s.BookingKey)
	digest.Write([]byte(owner + "\x00" + code))
	return hex.EncodeToString(digest.Sum(nil))
}

func (s Server) booking(w http.ResponseWriter, r *http.Request, owner string) error {
	if len(s.BookingKey) == 0 {
		return core.ErrNotFound
	}
	var input struct {
		Locator string `json:"locator"`
	}
	if err := parse(w, r, &input, 1024); err != nil {
		return err
	}
	digest := s.bookingDigest(owner, strings.ToUpper(strings.TrimSpace(input.Locator)))
	if digest == "" {
		return errInvalid
	}
	flights, err := s.Store.BookedFlights(r.Context(), owner, digest)
	if err != nil {
		return err
	}
	respond(w, 200, map[string]any{"flights": flights})
	return nil
}
