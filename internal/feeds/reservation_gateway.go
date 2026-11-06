package feeds

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxReservationResponseBytes = 16 << 10
	maxReservationLegs          = 8
	maxReservationRequestTime   = 6 * time.Second
)

var (
	reservationCarrierPattern = regexp.MustCompile(`^(?:[A-Z0-9]{2}|[A-Z]{3})$`)
	reservationLocatorPattern = regexp.MustCompile(`^[A-Z0-9]{6}$`)
	reservationFlightPattern  = regexp.MustCompile(`^(?:[A-Z0-9]{2}|[A-Z]{3})[0-9]{1,5}[A-Z]?$`)
	reservationBearerPattern  = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)
)

type ReservationQuery struct {
	Carrier string `json:"carrier"`
	Locator string `json:"locator"`
	Surname string `json:"surname"`
}

// ReservationLeg contains only a flight designator and its local departure date.
type ReservationLeg struct {
	Number string `json:"number"`
	Date   string `json:"date"`
}

type ReservationSource interface {
	Lookup(context.Context, ReservationQuery) ([]ReservationLeg, error)
}

// ReservationGateway calls a fixed, operator-managed bridge that has authorized
// reservation-system access. The bridge owns reservation verification.
type ReservationGateway struct {
	endpoint string
	token    string
	client   *http.Client
}

func NewReservationGateway(endpoint, token string, client *http.Client) (*ReservationGateway, error) {
	endpoint = strings.TrimSpace(endpoint)
	if !validFeedEndpoint(endpoint) {
		return nil, fmt.Errorf("%w: reservation gateway requires HTTPS or literal loopback HTTP without URL credentials", ErrInvalidQuery)
	}
	if len(token) == 0 || len(token) > 4096 || !reservationBearerPattern.MatchString(token) {
		return nil, fmt.Errorf("%w: invalid reservation gateway bearer credential", ErrInvalidQuery)
	}
	bounded := feedClient(client)
	bounded.Timeout = min(bounded.Timeout, maxReservationRequestTime)
	bounded.Jar = nil
	return &ReservationGateway{endpoint: endpoint, token: token, client: bounded}, nil
}

// NormalizeReservationQuery validates a booking credential without exposing
// any of its values in errors. The bridge receives the original surname case.
func NormalizeReservationQuery(input ReservationQuery) (ReservationQuery, error) {
	if len(input.Carrier) > 16 || len(input.Locator) > 16 || len(input.Surname) > 320 {
		return ReservationQuery{}, ErrInvalidQuery
	}
	input.Carrier = strings.ToUpper(strings.TrimSpace(input.Carrier))
	input.Locator = strings.ToUpper(strings.TrimSpace(input.Locator))
	if strings.ContainsFunc(input.Surname, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == '\u2028' || r == '\u2029'
	}) {
		return ReservationQuery{}, ErrInvalidQuery
	}
	input.Surname = strings.TrimSpace(input.Surname)
	if !reservationCarrierPattern.MatchString(input.Carrier) || !reservationLocatorPattern.MatchString(input.Locator) ||
		!utf8.ValidString(input.Surname) || utf8.RuneCountInString(input.Surname) > 80 ||
		!strings.ContainsFunc(input.Surname, unicode.IsLetter) || strings.ContainsFunc(input.Surname, invalidSurnameRune) {
		return ReservationQuery{}, ErrInvalidQuery
	}
	return input, nil
}

func invalidSurnameRune(r rune) bool {
	if unicode.IsLetter(r) || unicode.IsMark(r) || unicode.Is(unicode.Zs, r) || unicode.Is(unicode.Pd, r) {
		return false
	}
	switch r {
	case '\'', '’', '.', '·':
		return false
	default:
		return true
	}
}

func (g *ReservationGateway) Lookup(ctx context.Context, input ReservationQuery) ([]ReservationLeg, error) {
	query, err := NormalizeReservationQuery(input)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Version int `json:"version"`
		ReservationQuery
	}{Version: 1, ReservationQuery: query})
	if err != nil {
		return nil, ErrInvalidQuery
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+g.token)
	request.Header.Set("Cache-Control", "no-store")
	response, err := g.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: reservation gateway request failed", ErrUnavailable)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNotFound:
		return nil, ErrNotFound
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("%w: reservation gateway returned HTTP %d", ErrUnavailable, response.StatusCode)
	}

	kind, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		return nil, fmt.Errorf("%w: invalid reservation gateway content type", ErrUnavailable)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxReservationResponseBytes+1))
	if err != nil || len(raw) > maxReservationResponseBytes {
		return nil, fmt.Errorf("%w: invalid reservation gateway response body", ErrUnavailable)
	}
	return reservationLegs(raw, query.Locator)
}

func reservationLegs(raw []byte, locator string) ([]ReservationLeg, error) {
	invalid := fmt.Errorf("%w: invalid reservation gateway response", ErrUnavailable)
	var response struct {
		Version    int              `json:"version"`
		Authorized *bool            `json:"authorized"`
		Legs       []ReservationLeg `json:"legs"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || response.Version != 1 || response.Authorized == nil ||
		response.Legs == nil || len(response.Legs) > maxReservationLegs {
		return nil, invalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, invalid
	}
	if !*response.Authorized || len(response.Legs) == 0 {
		return nil, ErrNotFound
	}
	seen := make(map[ReservationLeg]bool)
	legs := make([]ReservationLeg, 0, len(response.Legs))
	for _, leg := range response.Legs {
		date, err := time.Parse(time.DateOnly, leg.Date)
		if !reservationFlightPattern.MatchString(leg.Number) || leg.Number == locator || err != nil || date.Year() == 0 {
			return nil, invalid
		}
		if !seen[leg] {
			legs = append(legs, leg)
			seen[leg] = true
		}
	}
	return legs, nil
}
