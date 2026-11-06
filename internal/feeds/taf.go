package feeds

import (
	"cmp"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	MaxTAFStations = MaxMETARStations
	tafEndpoint    = "https://aviationweather.gov/api/data/taf"
	maxTAFBytes    = 512 << 10
	maxTAFGroups   = 64
)

// TAF reads terminal aerodrome forecasts from NOAA's Aviation Weather Center.
type TAF struct {
	client *http.Client
}

func NewTAF(client *http.Client) *TAF {
	return &TAF{client: awcWeatherClient(client)}
}

// TAFPeriod is an adverse terminal forecast over the half-open UTC interval
// [StartsAt, EndsAt). Probability is zero when NOAA supplies no percentage.
type TAFPeriod struct {
	Airport     string
	Conditions  string
	Qualifier   string
	IssuedAt    time.Time
	StartsAt    time.Time
	EndsAt      time.Time
	Probability int
}

type tafReport struct {
	Station    string     `json:"icaoId"`
	Issued     string     `json:"issueTime"`
	Received   string     `json:"dbPopTime"`
	MostRecent int        `json:"mostRecent"`
	From       int64      `json:"validTimeFrom"`
	To         int64      `json:"validTimeTo"`
	Raw        string     `json:"rawTAF"`
	Groups     []tafGroup `json:"fcsts"`
}

type tafGroup struct {
	From               int64  `json:"timeFrom"`
	To                 int64  `json:"timeTo"`
	BecomingEnd        int64  `json:"timeBec"`
	Change             string `json:"fcstChange"`
	Probability        int    `json:"probability"`
	VerticalVisibility *int   `json:"vertVis"`
	WindShearHeight    int    `json:"wshearHgt"`
	awcWeatherValues
}

// Forecasts returns adverse periods from the latest received product for each
// requested station. Clear products and cancellations supersede prior issues.
func (t *TAF) Forecasts(ctx context.Context, stations []string, now time.Time) ([]TAFPeriod, error) {
	ids, err := awcWeatherStations(stations)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	query := url.Values{"ids": {strings.Join(ids, ",")}, "format": {"json"}, "time": {"issue"}}
	reports, err := readAWCWeatherReports[tafReport](ctx, t.client, tafEndpoint, "TAF", query, maxTAFBytes)
	if err != nil {
		return nil, err
	}
	type issue struct {
		report           tafReport
		issued, received time.Time
	}
	latest := make(map[string]issue, len(ids))
	for _, report := range reports {
		_, requested := slices.BinarySearch(ids, report.Station)
		issued, received := awcWeatherTime(report.Issued).UTC(), awcWeatherTime(report.Received).UTC()
		if !requested || issued.IsZero() || issued.After(now) || received.After(now) {
			continue
		}
		previous, found := latest[report.Station]
		if !found || issued.After(previous.issued) || issued.Equal(previous.issued) &&
			(received.After(previous.received) || received.Equal(previous.received) && report.MostRecent > previous.report.MostRecent) {
			latest[report.Station] = issue{report, issued, received}
		}
	}
	var result []TAFPeriod
	for _, id := range ids {
		if current, found := latest[id]; found {
			result = current.report.appendPeriods(result, current.issued, now)
		}
	}
	return result, nil
}

func (r tafReport) appendPeriods(result []TAFPeriod, issued, now time.Time) []TAFPeriod {
	if r.From <= 0 || r.To <= now.Unix() || r.To <= r.From || r.To <= issued.Unix() ||
		r.To-r.From > int64(48*time.Hour/time.Second) || len(r.Groups) > maxTAFGroups {
		return result
	}
	for word := range strings.FieldsSeq(r.Raw) {
		if word := strings.TrimSuffix(word, "="); word == "CNL" || word == "NIL" {
			return result
		}
	}
	slices.SortStableFunc(r.Groups, func(a, b tafGroup) int { return cmp.Compare(a.From, b.From) })
	add := func(from, to int64, conditions, qualifier string, probability int) {
		if conditions != "" && from < to && to > now.Unix() {
			result = append(result, TAFPeriod{
				Airport: r.Station, Conditions: conditions, Qualifier: qualifier, IssuedAt: issued,
				StartsAt: time.Unix(from, 0).UTC(), EndsAt: time.Unix(to, 0).UTC(), Probability: probability,
			})
		}
	}
	var priorEnd int64
	var priorConditions string
	for _, group := range r.Groups {
		from, to := max(group.From, r.From), min(group.To, r.To)
		if group.From <= 0 || from >= to || group.Probability < 0 || group.Probability >= 100 {
			continue
		}
		conditions := group.adverseConditions()
		switch group.Change {
		case "", "FM":
			if group.Probability == 0 {
				add(from, to, conditions, "prevailing", 0)
				priorEnd, priorConditions = to, conditions
			}
		case "BECMG":
			if group.BecomingEnd <= group.From || group.Probability != 0 {
				continue
			}
			// AWC fills in unchanged fields in BECMG. The resulting conditions
			// become prevailing at timeBec, with uncertain timing before then.
			transitionEnd := min(group.BecomingEnd, to)
			transitionConditions := conditions
			if priorEnd == from && priorConditions != "" && priorConditions != conditions {
				if conditions == "" {
					transitionConditions = priorConditions + " may persist during the transition"
				} else {
					transitionConditions = priorConditions + " changing to " + conditions
				}
			}
			add(from, transitionEnd, transitionConditions, "becoming", 0)
			add(max(from, transitionEnd), to, conditions, "prevailing", 0)
			priorEnd, priorConditions = to, conditions
		case "TEMPO":
			add(from, to, conditions, "temporary", group.Probability)
		case "PROB":
			if group.Probability != 0 {
				add(from, to, conditions, "probability", group.Probability)
			}
		}
	}
	return result
}

func (g tafGroup) adverseConditions() string {
	if g.VerticalVisibility != nil {
		g.CloudLayers = append(g.CloudLayers, awcWeatherCloud{Cover: "VV", Base: g.VerticalVisibility})
	}
	conditions := g.awcWeatherValues.adverseConditions()
	if g.WindShearHeight <= 0 || g.WindShearHeight > 2000 {
		return conditions
	}
	if conditions == "" {
		return "low-level wind shear"
	}
	return conditions + ", low-level wind shear"
}
