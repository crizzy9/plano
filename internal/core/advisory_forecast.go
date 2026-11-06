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
	StateHash  string `json:"stateHash,omitempty"`
}

func advisoryEvidence(advisory feeds.Advisory) forecastAdvisory {
	return forecastAdvisory{Source: advisory.Source, ID: advisory.ID, ObservedNS: advisory.ObservedAt.UnixNano(), StateHash: advisoryStateHash(advisory)}
}

func advisoryStateHash(advisory feeds.Advisory) string {
	advisory.ObservedAt = time.Time{}
	if advisory.StartsAt != nil {
		advisory.StartsAt = new(advisory.StartsAt.UTC())
	}
	if advisory.EndsAt != nil {
		advisory.EndsAt = new(advisory.EndsAt.UTC())
	}
	raw, err := json.Marshal(advisory)
	if err != nil {
		return ""
	}
	return hash(string(raw))
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
	return advisoryEvidenceRevised(ctx, db, `SELECT json_extract(value,'$.source'), json_extract(value,'$.id'),
 json_extract(value,'$.observedNs'), COALESCE(json_extract(value,'$.stateHash'),'')
FROM json_each(?) WHERE json_extract(value,'$.source')<>'' AND json_extract(value,'$.id')<>''`, string(evidence), now)
}

func advisoryEvidenceRevised(ctx context.Context, db rowQueryer, evidenceSQL string, evidence any, now time.Time) (bool, error) {
	var raw []byte
	err := db.QueryRowContext(ctx, `WITH evidence(source,id,observed_ns,state_hash) AS (`+evidenceSQL+`),
candidates AS (
 SELECT e.source,e.id,e.state_hash,a.data,a.observed_ns,1 AS signed FROM evidence e JOIN pushed_advisories a USING(source,id)
 WHERE a.observed_ns>=e.observed_ns AND a.observed_ns<=? AND (e.state_hash<>'' OR a.observed_ns>e.observed_ns)
 UNION ALL
 SELECT e.source,e.id,e.state_hash,q.data,q.observed_ns,1 AS signed
 FROM evidence e JOIN pushed_advisory_predecessors q USING(source,id) JOIN pushed_advisories p USING(source,id)
 WHERE p.observed_ns>? AND q.observed_ns>=e.observed_ns AND q.observed_ns<=? AND (e.state_hash<>'' OR q.observed_ns>e.observed_ns)
 UNION ALL
 SELECT e.source,e.id,e.state_hash,a.data,a.observed_ns,0 AS signed FROM evidence e JOIN polled_flight_notices a USING(source,id)
 WHERE a.observed_ns>=e.observed_ns AND a.observed_ns<=? AND (e.state_hash<>'' OR a.observed_ns>e.observed_ns)
), latest AS (
 SELECT *, ROW_NUMBER() OVER(PARTITION BY source,id,state_hash ORDER BY observed_ns DESC,signed DESC) AS revision_rank FROM candidates
)
SELECT json_group_array(json_object('stateHash',state_hash,'advisory',json(data))) FROM latest WHERE revision_rank=1`,
		evidence, now.UnixNano(), now.UnixNano(), now.UnixNano(), now.UnixNano()).Scan(&raw)
	if err != nil {
		return false, err
	}
	var revisions []struct {
		StateHash string         `json:"stateHash"`
		Advisory  feeds.Advisory `json:"advisory"`
	}
	if err := json.Unmarshal(raw, &revisions); err != nil {
		return false, err
	}
	for _, revision := range revisions {
		if revision.StateHash == "" || advisoryStateHash(revision.Advisory) != revision.StateHash {
			return true, nil
		}
		// Evidence stored before the timing guard can match the latest row
		// while still using a conflicting or expired published closure window.
		a := revision.Advisory
		if a.Kind == "airport_closure" {
			if decoded, ok := feeds.DecodeNOTAM(a.RawReason); ok && decoded.HasValidity &&
				(!decoded.FullAirportClosure || !decoded.MatchesValidity(a.StartsAt, a.EndsAt) || !decoded.EndsAt.After(now)) {
				return true, nil
			}
		}
	}
	return false, nil
}
