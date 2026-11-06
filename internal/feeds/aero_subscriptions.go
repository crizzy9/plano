package feeds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	aeroAlertIdent   = regexp.MustCompile(`^[A-Z]{3}[0-9]{1,5}[A-Z]?$\z`)
	aeroAlertAirport = regexp.MustCompile(`^[A-Z]{4}$\z`)
)

// AeroAlertScope is a departure-local route-day filter. AeroAPI v4.17.1
// documents these registration fields but no exact fa_flight_id filter.
type AeroAlertScope struct {
	Ident       string `json:"ident"`
	Origin      string `json:"origin"`
	Destination string `json:"destination"`
	Date        string `json:"date"`
}

// AeroAlertScope derives only scopes with a known operating ICAO designator,
// two ICAO airport codes, and a verified origin timezone.
func (f Flight) AeroAlertScope() (AeroAlertScope, time.Time, bool) {
	if f.Source != aeroSource && f.Source != "flightaware_schedule" || f.ScheduledDeparture == nil || f.ScheduledDeparture.IsZero() || f.OriginTimeZone == "" {
		return AeroAlertScope{}, time.Time{}, false
	}
	ident := f.ICAONumber
	if ident == "" && f.Source == "flightaware_schedule" && strings.HasPrefix(f.ID, schedulePrefix) {
		if key, err := parseScheduleKey(f.ID); err == nil {
			ident = key.OperatingNumber
		}
	}
	location, err := time.LoadLocation(f.OriginTimeZone)
	if err != nil {
		return AeroAlertScope{}, time.Time{}, false
	}
	date := f.ScheduledDeparture.In(location)
	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, location)
	scope := AeroAlertScope{Ident: ident, Origin: f.OriginICAO, Destination: f.DestinationICAO, Date: date.Format(time.DateOnly)}
	return scope, start, scope.valid()
}

func (s AeroAlertScope) valid() bool {
	day, err := time.Parse(time.DateOnly, s.Date)
	return err == nil && day.Year() >= 2000 && day.Year() <= 2100 && aeroAlertIdent.MatchString(s.Ident) &&
		aeroAlertAirport.MatchString(s.Origin) && aeroAlertAirport.MatchString(s.Destination) && s.Origin != s.Destination
}

type AeroSubscription struct {
	ID              int64  `json:"id"`
	Ident           string `json:"ident"`
	IdentICAO       string `json:"ident_icao"`
	UserIdent       string `json:"user_ident"`
	Origin          string `json:"origin"`
	OriginICAO      string `json:"origin_icao"`
	Destination     string `json:"destination"`
	DestinationICAO string `json:"destination_icao"`
	Start           string `json:"start"`
	End             string `json:"end"`
	TargetURL       string `json:"target_url"`
}

func (a AeroSubscription) MatchesScope(scope AeroAlertScope) bool {
	return (a.Ident == scope.Ident || a.IdentICAO == scope.Ident || a.UserIdent == scope.Ident) &&
		(a.Origin == scope.Origin || a.OriginICAO == scope.Origin) &&
		(a.Destination == scope.Destination || a.DestinationICAO == scope.Destination) &&
		a.Start == scope.Date && a.End == scope.Date
}

// AeroAlertEndpointConfigured checks the upstream account prerequisite without
// returning its URL or modifying any account-wide alert settings.
func (a *AeroAPI) AeroAlertEndpointConfigured(ctx context.Context) (bool, error) {
	var response struct {
		URL string `json:"url"`
	}
	if err := a.getJSON(ctx, "/alerts/endpoint", nil, &response); err != nil {
		if errors.Is(err, ErrInvalidQuery) { // Documented no-default-endpoint response.
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(response.URL) != "", nil
}

func (a *AeroAPI) ListAeroSubscriptions(ctx context.Context, cursor string) ([]AeroSubscription, string, error) {
	if len(cursor) > 2048 {
		return nil, "", ErrInvalidQuery
	}
	query := url.Values{"max_pages": {"1"}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	var response struct {
		Alerts []AeroSubscription `json:"alerts"`
		Links  struct {
			Next string `json:"next"`
		} `json:"links"`
	}
	if err := a.getJSON(ctx, "/alerts", query, &response); err != nil {
		return nil, "", err
	}
	if response.Alerts == nil || len(response.Alerts) > 2048 {
		return nil, "", fmt.Errorf("%w: invalid AeroAPI alert inventory", ErrUnavailable)
	}
	if response.Links.Next == "" {
		return response.Alerts, "", nil
	}
	next, ok := a.alertReference(response.Links.Next)
	if !ok || next.Path != "/alerts" || len(next.Query()["cursor"]) != 1 || len(next.Query().Get("cursor")) > 2048 || next.Query().Get("cursor") == "" || next.Query().Get("cursor") == cursor {
		return nil, "", fmt.Errorf("%w: invalid AeroAPI alert inventory cursor", ErrUnavailable)
	}
	return response.Alerts, next.Query().Get("cursor"), nil
}

func (a *AeroAPI) InspectAeroSubscription(ctx context.Context, id int64) (AeroSubscription, error) {
	if id <= 0 {
		return AeroSubscription{}, ErrInvalidQuery
	}
	var result AeroSubscription
	if err := a.getJSON(ctx, "/alerts/"+strconv.FormatInt(id, 10), nil, &result); err != nil {
		return AeroSubscription{}, err
	}
	if result.ID != id {
		return AeroSubscription{}, fmt.Errorf("%w: AeroAPI alert identity mismatch", ErrUnavailable)
	}
	return result, nil
}

// CreateAeroSubscription sends one bounded route-day alert request. Callers
// must persist their recovery marker before calling. AeroAPI offers no
// documented idempotency key, and a failed response may follow remote creation.
func (a *AeroAPI) CreateAeroSubscription(ctx context.Context, scope AeroAlertScope, targetURL string) (int64, error) {
	target, err := url.Parse(targetURL)
	if !scope.valid() || err != nil || target.Scheme != "https" || target.Hostname() == "" || target.Fragment != "" || target.Opaque != "" || len(targetURL) > 4096 {
		return 0, ErrInvalidQuery
	}
	body, err := json.Marshal(struct {
		Ident       string          `json:"ident"`
		Origin      string          `json:"origin"`
		Destination string          `json:"destination"`
		Start       string          `json:"start"`
		End         string          `json:"end"`
		TargetURL   string          `json:"target_url"`
		MaxWeekly   int             `json:"max_weekly"`
		Events      map[string]bool `json:"events"`
	}{scope.Ident, scope.Origin, scope.Destination, scope.Date, scope.Date, targetURL, 64,
		map[string]bool{"arrival": true, "cancelled": true, "departure": true, "diverted": true, "filed": false, "out": true, "off": true, "on": true, "in": true}})
	if err != nil {
		return 0, err
	}
	headers, err := a.writeAeroAlert(ctx, http.MethodPost, "/alerts", body, http.StatusCreated)
	if err != nil {
		return 0, err
	}
	location, ok := a.alertReference(headers.Get("Location"))
	if !ok || location.RawQuery != "" {
		return 0, fmt.Errorf("%w: AeroAPI created an alert without a valid Location", ErrUnavailable)
	}
	encodedID, ok := strings.CutPrefix(location.Path, "/alerts/")
	id, err := strconv.ParseInt(encodedID, 10, 64)
	if !ok || err != nil || id <= 0 || strconv.FormatInt(id, 10) != encodedID {
		return 0, fmt.Errorf("%w: AeroAPI returned an invalid alert Location", ErrUnavailable)
	}
	return id, nil
}

// DeleteAeroSubscription is only for a previously inspected Plano-owned ID.
func (a *AeroAPI) DeleteAeroSubscription(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrInvalidQuery
	}
	_, err := a.writeAeroAlert(ctx, http.MethodDelete, "/alerts/"+strconv.FormatInt(id, 10), nil, http.StatusNoContent)
	return err
}

func (a *AeroAPI) writeAeroAlert(ctx context.Context, method, path string, body []byte, want int) (http.Header, error) {
	if a.key == "" {
		return nil, fmt.Errorf("%w: FlightAware AeroAPI key is missing", ErrUnavailable)
	}
	request, err := http.NewRequestWithContext(ctx, method, a.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, ErrInvalidQuery
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-apikey", a.key)
	if len(body) != 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: AeroAPI alert request failed", ErrUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		class := ErrUnavailable
		if response.StatusCode == http.StatusBadRequest {
			class = ErrInvalidQuery
		} else if response.StatusCode == http.StatusNotFound {
			class = ErrNotFound
		}
		return nil, fmt.Errorf("%w: AeroAPI alert request returned HTTP %d", class, response.StatusCode)
	}
	return response.Header, nil
}

// alertReference accepts only same-origin alert paths and extracts data from
// them. Neither Location nor an inventory link is followed as an HTTP URL.
func (a *AeroAPI) alertReference(raw string) (*url.URL, bool) {
	base, baseErr := url.Parse(a.base)
	ref, err := url.Parse(raw)
	if baseErr != nil || err != nil || raw == "" || ref.User != nil || ref.Fragment != "" || ref.Opaque != "" ||
		ref.Host != "" && !strings.EqualFold(ref.Host, base.Host) || ref.Scheme != "" && !strings.EqualFold(ref.Scheme, base.Scheme) {
		return nil, false
	}
	ref.Path = strings.TrimPrefix(ref.Path, strings.TrimRight(base.Path, "/")+"/")
	if ref.Path == "" {
		ref.Path = "/alerts"
	} else if !strings.HasPrefix(ref.Path, "/") {
		ref.Path = "/" + ref.Path
	}
	return ref, true
}
