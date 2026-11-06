package api

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/itinerary"
)

var tripItPath = regexp.MustCompile(`^/feed/ical/(?:[A-Za-z0-9_-]+/){0,4}[A-Za-z0-9_.-]+\.ics$`)

func tripItURL(raw string) (*url.URL, error) {
	if len(raw) > 1024 {
		return nil, errInvalid
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "webcal") || u.Host != "www.tripit.com" && u.Host != "tripit.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !tripItPath.MatchString(u.Path) || strings.Contains(u.Path, "..") {
		return nil, errInvalid
	}
	u.Scheme, u.Host = "https", "www.tripit.com"
	return u, nil
}

var tripItClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func downloadTripIt(ctx context.Context, client *http.Client, target *url.URL) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", errInvalid
	}
	request.Header.Set("Accept", "text/calendar")
	response, err := client.Do(request)
	if err != nil {
		return "", feeds.ErrUnavailable
	} // A URL error may contain the private feed token.
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnauthorized {
		return "", feeds.ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return "", feeds.ErrUnavailable
	}
	const maxFeed = 256 << 10
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxFeed+1))
	if err != nil || len(raw) > maxFeed {
		return "", feeds.ErrUnavailable
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(text)), "BEGIN:VCALENDAR") {
		return "", feeds.ErrUnavailable
	}
	return text, nil
}

func (s Server) importTripIt(w http.ResponseWriter, r *http.Request, owner string) error {
	var input struct {
		URL    string `json:"url"`
		Offset int    `json:"offset"`
	}
	if err := parse(w, r, &input, 2048); err != nil {
		return err
	}
	if input.Offset < 0 || input.Offset > 1024 {
		return errInvalid
	}
	target, err := tripItURL(input.URL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	calendar, err := downloadTripIt(ctx, tripItClient, target)
	if err != nil {
		return err
	}
	parsed, err := itinerary.SubscriptionCalendar(calendar)
	if err != nil {
		return errInvalid
	}
	page := upcomingTripItPage(parsed, time.Now().UTC().Format(time.DateOnly), input.Offset)
	var next *int
	if input.Offset+len(page.Legs) < len(parsed.Legs) {
		offset := input.Offset + len(page.Legs)
		next = &offset
	}
	result, err := s.resolveItinerary(ctx, page)
	if err != nil {
		return err
	}
	if _, err := s.Store.CommitImport(ctx, owner, result.Flights, s.bookingDigest(owner, parsed.BookingCode), "", ""); err != nil {
		return err
	}
	result.NextOffset = next
	respond(w, 200, result)
	return nil
}

func upcomingTripItPage(parsed itinerary.Itinerary, today string, offset int) itinerary.Itinerary {
	sort.SliceStable(parsed.Legs, func(i, j int) bool {
		a, b := parsed.Legs[i].Date, parsed.Legs[j].Date
		aUpcoming, bUpcoming := a >= today, b >= today
		if aUpcoming != bUpcoming {
			return aUpcoming
		}
		if aUpcoming {
			return a < b
		}
		return a > b
	})
	offset = min(offset, len(parsed.Legs))
	parsed.Legs = parsed.Legs[offset:min(len(parsed.Legs), offset+itinerary.MaxFlights)]
	return parsed
}
