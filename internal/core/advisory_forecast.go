package core

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const advisoryForecastTTL = 30 * time.Minute

type forecastAdvisory struct {
	Source     string `json:"source"`
	ID         string `json:"id"`
	ObservedNS int64  `json:"observedNs"`
}

func advisoryEvidence(advisory feeds.Advisory) forecastAdvisory {
	return forecastAdvisory{Source: advisory.Source, ID: advisory.ID, ObservedNS: advisory.ObservedAt.UnixNano()}
}

func advisoryForecastValidity(advisory feeds.Advisory, now time.Time) time.Time {
	if advisory.ObservedAt.IsZero() || advisory.ObservedAt.After(now) || advisoryCancelled(advisory.Kind) {
		return time.Time{}
	}
	if advisory.EndsAt == nil {
		return advisory.ObservedAt.Add(advisoryForecastTTL)
	}
	if advisory.StartsAt != nil && !advisory.EndsAt.After(*advisory.StartsAt) {
		return time.Time{}
	}
	return earliestTime(now.Add(advisoryForecastTTL), earliestTime(*advisory.EndsAt, advisory.ObservedAt.Add(maxNoticeAge)))
}

func advisoryCancelled(kind string) bool {
	switch kind {
	case "cancelled", "canceled", "cancellation", "notam_cancelled", "notam_canceled", "notam_cancellation":
		return true
	default:
		return false
	}
}

func earliestTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func earliestValidity(current *time.Time, candidate time.Time) *time.Time {
	if current == nil || candidate.Before(*current) {
		return &candidate
	}
	return current
}

func legacyAdvisoryReason(reason string) bool {
	return !strings.HasPrefix(reason, "In the historical model, ") && strings.Contains(reason, " reports ") && strings.Contains(reason, " at ")
}

func forecastAdvisoryRevised(ctx context.Context, db rowQueryer, forecast *Prediction, now time.Time) (bool, error) {
	if len(forecast.AdvisoryEvidence) == 0 {
		return false, nil
	}
	evidence, err := json.Marshal(forecast.AdvisoryEvidence)
	if err != nil {
		return false, err
	}
	var newer bool
	err = db.QueryRowContext(ctx, `WITH evidence AS (
 SELECT json_extract(value,'$.source') AS source, json_extract(value,'$.id') AS id,
        json_extract(value,'$.observedNs') AS observed_ns
 FROM json_each(?) WHERE json_extract(value,'$.source')<>'' AND json_extract(value,'$.id')<>''
)
SELECT EXISTS (
 SELECT 1 FROM evidence e JOIN pushed_advisories a USING(source,id)
 WHERE a.observed_ns>e.observed_ns AND a.observed_ns<=?
 UNION ALL
 SELECT 1 FROM evidence e JOIN polled_flight_notices a USING(source,id)
 WHERE a.observed_ns>e.observed_ns AND a.observed_ns<=?
)`, string(evidence), now.UnixNano(), now.UnixNano()).Scan(&newer)
	return newer, err
}
