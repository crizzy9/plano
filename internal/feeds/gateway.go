package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
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

func (g *AdvisoryGateway) AdvisorySourceID() string { return g.source }

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
		if !normalizeBridgeAdvisory(row, g.source) || seen[row.ID] {
			return nil, fmt.Errorf("%w: %s gateway returned an invalid advisory at index %d", ErrUnavailable, g.source, i)
		}
		applyNOTAMSummary(row)
		seen[row.ID] = true
	}
	return response.Advisories, nil
}
