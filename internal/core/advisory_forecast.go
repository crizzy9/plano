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
	Airport    string `json:"airport,omitempty"`
	ObservedNS int64  `json:"observedNs"`
	StateHash  string `json:"stateHash,omitempty"`
}

func advisoryEvidence(advisory feeds.Advisory) forecastAdvisory {
	return forecastAdvisory{Source: advisory.Source, ID: advisory.ID, Airport: advisory.Airport,
		ObservedNS: advisory.ObservedAt.UnixNano(), StateHash: advisoryStateHash(advisory)}
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

// advisoryOccurrence narrows a verified recurring closure to the published
// window containing the affected operation. Keep the original advisory for
// revision checks and treat the operation as pending at now if it is overdue.
func advisoryOccurrence(a feeds.Advisory, decoded feeds.NOTAMSummary, operation, now time.Time) (feeds.Advisory, bool) {
	if !decoded.HasDailyClosure() || a.Kind != "airport_closure" && a.Kind != "airport_restriction" {
		return a, true
	}
	if operation.Before(now) {
		operation = now
	}
	start, end, ok := decoded.NextDailyClosure(operation)
	if !ok || start.After(operation) {
		return feeds.Advisory{}, false
	}
	a.StartsAt, a.EndsAt = &start, &end
	return a, true
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
 json_extract(value,'$.observedNs'), COALESCE(json_extract(value,'$.stateHash'),''), COALESCE(json_extract(value,'$.airport'),'')
FROM json_each(?) WHERE json_extract(value,'$.source')<>'' AND json_extract(value,'$.id')<>''`, string(evidence), now, false)
}

func advisoryEvidenceRevised(ctx context.Context, db rowQueryer, evidenceSQL string, evidence any, now time.Time, requireCurrent bool) (bool, error) {
	var superseded bool
	var raw []byte
	t := now.UnixNano()
	err := db.QueryRowContext(ctx, `WITH evidence(source,id,observed_ns,state_hash,airport) AS (`+evidenceSQL+`),
candidates(source,id,evidence_ns,state_hash,airport,data,observed_ns,expires_ns,signed) AS (
 SELECT e.source,e.id,e.observed_ns,e.state_hash,e.airport,a.data,a.observed_ns,a.expires_ns,1 FROM evidence e JOIN pushed_advisories a USING(source,id)
 WHERE a.observed_ns>=e.observed_ns AND a.observed_ns<=? AND (e.state_hash<>'' OR a.observed_ns>e.observed_ns)
 UNION ALL
 SELECT e.source,e.id,e.observed_ns,e.state_hash,e.airport,q.data,q.observed_ns,q.expires_ns,1
 FROM evidence e JOIN pushed_advisory_predecessors q USING(source,id) JOIN pushed_advisories p USING(source,id)
 WHERE p.observed_ns>? AND q.observed_ns>=e.observed_ns AND q.observed_ns<=? AND (e.state_hash<>'' OR q.observed_ns>e.observed_ns)
 UNION ALL
 SELECT e.source,e.id,e.observed_ns,e.state_hash,e.airport,a.data,a.observed_ns,a.expires_ns,0 FROM evidence e JOIN polled_flight_notices a USING(source,id)
 WHERE a.observed_ns>=e.observed_ns AND a.observed_ns<=? AND (e.state_hash<>'' OR a.observed_ns>e.observed_ns)
), latest AS (
 SELECT *, ROW_NUMBER() OVER(PARTITION BY source,id,evidence_ns,state_hash,airport ORDER BY observed_ns DESC,signed DESC) AS revision_rank
 FROM candidates
), scoped AS (
 SELECT source,id,observed_ns,airport FROM evidence WHERE airport<>''
 UNION ALL
 SELECT source,id,evidence_ns,json_extract(data,'$.airport') FROM latest WHERE revision_rank=1 AND airport='' AND state_hash<>''
)
SELECT EXISTS(SELECT 1 FROM scoped e JOIN advisory_supersessions s
 ON s.source=e.source AND s.airport=e.airport AND s.predecessor_id=e.id
 AND s.observed_ns BETWEEN e.observed_ns AND ?),
 (SELECT json_group_array(json_object('stateHash',state_hash,'advisory',json(data),'expiresNs',expires_ns)) FROM latest WHERE revision_rank=1)`,
		evidence, t, t, t, t, t).Scan(&superseded, &raw)
	if err != nil || superseded {
		return superseded, err
	}
	var revisions []struct {
		StateHash string         `json:"stateHash"`
		Advisory  feeds.Advisory `json:"advisory"`
		ExpiresNS int64          `json:"expiresNs"`
	}
	if err := json.Unmarshal(raw, &revisions); err != nil {
		return false, err
	}
	if requireCurrent && len(revisions) == 0 {
		return true, nil
	}
	for _, revision := range revisions {
		if requireCurrent && revision.ExpiresNS <= t || revision.StateHash == "" || advisoryStateHash(revision.Advisory) != revision.StateHash {
			return true, nil
		}
		// Evidence stored before the timing guard can match the latest row
		// while still using a conflicting or expired published closure window.
		a := revision.Advisory
		if a.Kind == "airport_restriction" {
			if decoded, ok := feeds.DecodeNOTAM(a.RawReason); ok && decoded.HasDailyClosure() {
				return true, nil
			}
		} else if a.Kind == "airport_closure" {
			if decoded, ok := feeds.DecodeNOTAM(a.RawReason); ok && decoded.HasValidity &&
				(!decoded.FullAirportClosure || !decoded.MatchesValidity(a.StartsAt, a.EndsAt) || !decoded.EndsAt.After(now)) {
				return true, nil
			}
		}
	}
	return false, nil
}
