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
	flights, err := s.lookup(ctx, leg.Number, leg.Date)
	if err != nil || leg.UTCDeparture.IsZero() {
		return flights, err
	}
	matching := func(flights []feeds.Flight) []feeds.Flight {
		var found []feeds.Flight
		for _, f := range flights {
			if f.ScheduledDeparture != nil && f.ScheduledDeparture.Sub(leg.UTCDeparture).Abs() <= 45*time.Minute {
				found = append(found, f)
			}
		}
		return found
	}
	if candidates := matching(flights); len(candidates) != 0 {
		return candidates, nil
	}
	// The UTC calendar date can be a day apart from the departure airport's date.
	var candidates []feeds.Flight
	for _, offset := range []int{-1, 1} {
		adjacent, err := s.lookup(ctx, leg.Number, leg.UTCDeparture.AddDate(0, 0, offset).Format(time.DateOnly))
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, matching(adjacent)...)
	}
	return candidates, nil
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
		respond(w, 413, map[string]string{"error": "could not read email within size limit"})
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
	result, err := s.resolveItinerary(ctx, parsed)
	if err != nil {
		s.failure(w, err)
		return
	}
	if len(result.Flights) == 0 {
		respond(w, 422, map[string]string{"error": "email has no uniquely matching flights"})
		return
	}
	partial := len(result.Unmatched) > 0
	if partial {
		digest = ""
	}
	if _, err := s.Store.CommitImport(ctx, owner, result.Flights, s.bookingDigest(owner, parsed.BookingCode), digest, token); err != nil {
		s.failure(w, err)
		return
	}
	if partial {
		w.Header().Set("Retry-After", "300")
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "some flights could not yet be matched"})
		return
	}
	w.WriteHeader(204)
}

var bookingPattern = regexp.MustCompile(`^[A-Z0-9]{6}$`)

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
