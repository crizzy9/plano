package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const (
	FAASWIM     = "faa_swim"
	Eurocontrol = "eurocontrol_nm"
)

// AdvisoryGateway consumes a normalized snapshot from an operator-managed
// bridge with authorized access to FAA SCDS or EUROCONTROL NM B2B.
// The bridge owns the upstream subscription and schema mapping.
type AdvisoryGateway struct {
	endpoint string
	token    string
	source   string
	client   *http.Client
}

func NewAdvisoryGateway(endpoint, token, source string, client *http.Client) (*AdvisoryGateway, error) {
	if source != FAASWIM && source != Eurocontrol {
		return nil, fmt.Errorf("%w: unsupported advisory gateway source", ErrInvalidQuery)
	}
	if !validFeedEndpoint(endpoint) {
		return nil, fmt.Errorf("%w: advisory gateway requires HTTPS or loopback HTTP without URL credentials", ErrInvalidQuery)
	}
	if strings.ContainsAny(token, "\r\n") {
		return nil, fmt.Errorf("%w: invalid advisory gateway credential", ErrInvalidQuery)
	}
	return &AdvisoryGateway{endpoint: endpoint, token: strings.TrimSpace(token), source: source, client: feedClient(client)}, nil
}

func validFeedEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.ForceQuery {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	address := net.ParseIP(parsed.Hostname())
	return parsed.Scheme == "http" && address != nil && address.IsLoopback()
}

var gatewayKind = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$\z`)

func (g *AdvisoryGateway) Advisories(ctx context.Context) ([]Advisory, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if g.token != "" {
		request.Header.Set("Authorization", "Bearer "+g.token)
	}
	body, err := responseBody(g.client, request, g.source)
	if err != nil {
		return nil, err
	}
	var response struct {
		Version    int        `json:"version"`
		Advisories []Advisory `json:"advisories"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Version != 1 || response.Advisories == nil || len(response.Advisories) > 4096 {
		return nil, fmt.Errorf("%w: %s gateway returned an invalid snapshot", ErrUnavailable, g.source)
	}
	seen := make(map[string]bool)
	for i := range response.Advisories {
		row := &response.Advisories[i]
		row.ID = strings.TrimSpace(row.ID)
		row.Airport = strings.ToUpper(strings.TrimSpace(row.Airport))
		row.Area = strings.TrimSpace(row.Area)
		if row.ID == "" || len(row.ID) > 128 || seen[row.ID] || !gatewayKind.MatchString(row.Kind) || row.ObservedAt.IsZero() ||
			row.Airport == "" && row.Area == "" || row.Airport != "" && !airportCode.MatchString(row.Airport) || len(row.Area) > 128 ||
			len(row.Reason) > 4096 || len(row.RawReason) > 8192 ||
			row.Direction != "" && row.Direction != "arrival" && row.Direction != "departure" ||
			row.DelayMinutes != nil && (*row.DelayMinutes < 0 || *row.DelayMinutes > 1440) ||
			row.StartsAt != nil && row.EndsAt != nil && !row.EndsAt.After(*row.StartsAt) {
			return nil, fmt.Errorf("%w: %s gateway returned an invalid advisory at index %d", ErrUnavailable, g.source, i)
		}
		row.Source = g.source
		seen[row.ID] = true
	}
	return response.Advisories, nil
}
