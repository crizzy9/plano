package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	MaxMETARStations        = 24
	METARSource             = "noaa_awc_metar"
	METARWeatherObservation = "weather_observation"
	metarEndpoint           = "https://aviationweather.gov/api/data/metar"
	metarValidity           = 90 * time.Minute
	maxMETARBytes           = 128 << 10
)

var metarStationID = regexp.MustCompile(`^[A-Z]{4}$`)

// METAR reads terminal weather observations from NOAA's Aviation Weather Center.
type METAR struct {
	client *http.Client
}

func NewMETAR(client *http.Client) *METAR {
	return &METAR{client: awcWeatherClient(client)}
}

type metarReport struct {
	Station  string `json:"icaoId"`
	Observed int64  `json:"obsTime"`
	Received string `json:"receiptTime"`
	awcWeatherValues
}

// Observations returns current adverse conditions from the latest observation
// for each requested station. A missing station or a clear report has no entry.
func (m *METAR) Observations(ctx context.Context, stations []string, now time.Time) ([]Advisory, error) {
	ids, err := awcWeatherStations(stations)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	query := url.Values{"ids": {strings.Join(ids, ",")}, "format": {"json"}}
	reports, err := readAWCWeatherReports[metarReport](ctx, m.client, metarEndpoint, "METAR", query, maxMETARBytes)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]metarReport, len(ids))
	for _, report := range reports {
		_, requested := slices.BinarySearch(ids, report.Station)
		at := time.Unix(report.Observed, 0).UTC()
		if !requested || report.Observed <= 0 || at.After(now) || !at.Add(metarValidity).After(now) {
			continue
		}
		previous := latest[report.Station]
		if report.Observed > previous.Observed || report.Observed == previous.Observed && report.receivedAt().After(previous.receivedAt()) {
			latest[report.Station] = report
		}
	}
	var result []Advisory
	for _, id := range ids {
		report, ok := latest[id]
		if !ok {
			continue
		}
		reason := report.adverseConditions()
		if reason == "" {
			continue
		}
		observed := time.Unix(report.Observed, 0).UTC()
		expires := observed.Add(metarValidity)
		result = append(result, Advisory{
			ID: id + "-" + strconv.FormatInt(report.Observed, 10), Airport: id, Kind: METARWeatherObservation,
			Source: METARSource, ObservedAt: observed, StartsAt: &observed, EndsAt: &expires, Reason: reason,
		})
	}
	return result, nil
}

func awcWeatherClient(client *http.Client) *http.Client {
	bounded := feedClient(client)
	bounded.Timeout = min(bounded.Timeout, 8*time.Second)
	bounded.Jar = nil
	return bounded
}

func awcWeatherStations(stations []string) ([]string, error) {
	requested := make(map[string]bool)
	ids := make([]string, 0, min(len(stations), MaxMETARStations))
	for _, station := range stations {
		id := strings.ToUpper(strings.TrimSpace(station))
		if !metarStationID.MatchString(id) {
			return nil, ErrInvalidQuery
		}
		if !requested[id] {
			if len(ids) == MaxMETARStations {
				return nil, ErrInvalidQuery
			}
			requested[id] = true
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func readAWCWeatherReports[T any](ctx context.Context, client *http.Client, endpoint, product string, query url.Values, limit int64) ([]T, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Plano/1.0 (self-hosted flight tracker)")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s request failed", ErrUnavailable, product)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s returned HTTP %d", ErrUnavailable, product, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: invalid %s response length", ErrUnavailable, product)
	}
	var reports []T
	if err := json.Unmarshal(body, &reports); err != nil || reports == nil || len(reports) > 8*MaxMETARStations {
		return nil, fmt.Errorf("%w: invalid %s response", ErrUnavailable, product)
	}
	return reports, nil
}

func (r metarReport) receivedAt() time.Time {
	return awcWeatherTime(r.Received)
}

func awcWeatherTime(raw string) time.Time {
	value, _ := time.Parse(time.RFC3339Nano, strings.Replace(raw, " ", "T", 1))
	return value
}

type awcWeatherValues struct {
	Wind        int               `json:"wspd"`
	Gust        int               `json:"wgst"`
	Visibility  json.RawMessage   `json:"visib"`
	Weather     string            `json:"wxString"`
	CloudLayers []awcWeatherCloud `json:"clouds"`
}

type awcWeatherCloud struct {
	Cover string `json:"cover"`
	Base  *int   `json:"base"`
}

func (r awcWeatherValues) adverseConditions() string {
	var conditions []string
	for group := range strings.FieldsSeq(r.Weather) {
		condition := metarWeatherCondition(strings.TrimLeft(group, "+-"))
		if condition != "" && !slices.Contains(conditions, condition) {
			conditions = append(conditions, condition)
		}
	}
	// IFR ceiling and visibility bounds, expressed in the reported units.
	ceiling := 1000
	for _, layer := range r.CloudLayers {
		if layer.Base != nil && *layer.Base >= 0 && (layer.Cover == "BKN" || layer.Cover == "OVC" || layer.Cover == "OVX" || layer.Cover == "VV") {
			ceiling = min(ceiling, *layer.Base)
		}
	}
	if ceiling < 1000 {
		conditions = append(conditions, fmt.Sprintf("a low ceiling at %d ft", ceiling))
	}
	visibility, err := strconv.ParseFloat(strings.Trim(string(r.Visibility), `"`), 64)
	if err == nil && visibility >= 0 && visibility < 3 {
		conditions = append(conditions, fmt.Sprintf("low visibility at %g statute miles", visibility))
	}
	// These wind thresholds select which measured values to show. Local runway
	// conditions and aircraft operating limits are outside the observation.
	if r.Wind >= 25 && r.Wind <= 250 {
		conditions = append(conditions, fmt.Sprintf("sustained winds at %d kt", r.Wind))
	}
	if r.Gust >= 30 && r.Gust <= 250 {
		conditions = append(conditions, fmt.Sprintf("gusts up to %d kt", r.Gust))
	}
	return strings.Join(conditions, ", ")
}

func metarWeatherCondition(group string) string {
	switch {
	case strings.HasPrefix(group, "VCTS"):
		return "thunderstorms in the vicinity"
	case strings.HasPrefix(group, "TS"):
		return "thunderstorms"
	case strings.HasPrefix(group, "FZRA"):
		return "freezing rain"
	case strings.HasPrefix(group, "FZDZ"):
		return "freezing drizzle"
	case group == "FZFG":
		return "freezing fog"
	case group == "BLSN":
		return "blowing snow"
	case group == "DS":
		return "a dust storm"
	case group == "SS":
		return "a sandstorm"
	default:
		return ""
	}
}
