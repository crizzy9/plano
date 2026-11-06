package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/crizzy9/plano/internal/feeds"
)

const (
	maxFlightNotices   = 8
	maxPolledNotices   = 4096
	polledNoticeMaxAge = 30 * time.Minute
)

// FlightNotice is published airport context for an operation on the flight's
// route. A match establishes neither a flight-specific restriction nor a delay.
type FlightNotice struct {
	ID         string     `json:"id"`
	Airport    string     `json:"airport"`
	Operation  string     `json:"operation"`
	Kind       string     `json:"kind"`
	Source     string     `json:"source"`
	Summary    string     `json:"summary"`
	ObservedAt time.Time  `json:"observedAt"`
	ValidUntil time.Time  `json:"validUntil"`
	StartsAt   *time.Time `json:"startsAt,omitempty"`
	EndsAt     *time.Time `json:"endsAt,omitempty"`
}

// Only a successful snapshot changes this cache. Keep observation times as
// received, even when the same revision appears in repeated polls.
func (s *Store) cachePolledFlightNotices(ctx context.Context, source string, advisories []feeds.Advisory, polledAt time.Time) error {
	if source == "" {
		return nil
	}
	type entry struct {
		Data     feeds.Advisory `json:"data"`
		Observed int64          `json:"observed"`
		Expires  int64          `json:"expires"`
	}
	entries := make([]entry, 0, min(len(advisories), maxPolledNotices))
	var cancellations []feeds.Advisory
	var linked bool
	for _, a := range advisories {
		if len(entries)+len(cancellations) == maxPolledNotices {
			break
		}
		if a.Source != source || a.ID == "" || a.ObservedAt.IsZero() || s.ignoreLegacyTFMSReplay(a) {
			continue
		}
		if a.Kind == feeds.AdvisoryTFMSCancellation {
			if a.IsTFMSProgramCancellation() && a.ObservationAllowed(polledAt) {
				cancellations = append(cancellations, a)
			}
			continue
		}
		if a.SupersedesID != "" && a.ObservedAt.After(polledAt.Add(advisorySupersessionLead)) {
			return fmt.Errorf("%w: advisory replacement observation is too far ahead", feeds.ErrInvalidQuery)
		}
		linked = linked || a.SupersedesID != ""
		expires := earliestTime(a.Expiry(), polledAt.Add(polledNoticeMaxAge))
		entries = append(entries, entry{a, a.ObservedAt.UnixNano(), expires.UnixNano()})
	}
	payload, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	snapshot := string(payload)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, cancellation := range cancellations {
		if err := recordTFMSProgramCancellation(ctx, tx, cancellation); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM polled_flight_notices WHERE source=? AND id NOT IN (
SELECT json_extract(value,'$.data.id') FROM json_each(?))`, source, snapshot); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO polled_flight_notices(source,id,airport,observed_ns,expires_ns,data)
SELECT ?,json_extract(value,'$.data.id'),COALESCE(json_extract(value,'$.data.airport'),''),
       json_extract(value,'$.observed'),json_extract(value,'$.expires'),json_extract(value,'$.data')
FROM json_each(?) WHERE true
ON CONFLICT(source,id) DO UPDATE SET airport=excluded.airport,observed_ns=excluded.observed_ns,
  expires_ns=excluded.expires_ns,data=excluded.data
WHERE excluded.observed_ns>=polled_flight_notices.observed_ns`, source, snapshot); err != nil {
		return err
	}
	if linked {
		if err := storeAdvisorySupersessions(ctx, tx, `SELECT source,airport,json_extract(data,'$.supersedesId'),observed_ns,id FROM polled_flight_notices
WHERE source=? AND airport<>'' AND observed_ns<=? AND (observed_ns>=? OR
(observed_ns>=? AND source='faa_swim' AND (json_extract(data,'$.supersedesId') GLOB 'tfms/gs/*' OR json_extract(data,'$.supersedesId') GLOB 'tfms/gdp/*')))
AND json_extract(data,'$.supersedesId') NOT IN ('',id)`, source,
			polledAt.Add(advisorySupersessionLead).UnixNano(), polledAt.Add(-advisorySupersessionRetention).UnixNano(),
			polledAt.Add(-tfmsAdvisoryRetention).UnixNano()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FlightNotices projects the latest known airport notices for operations that
// have yet to take place. The API checks installation tracking before calling.
func (s *Store) FlightNotices(ctx context.Context, flight feeds.Flight, now time.Time) ([]FlightNotice, error) {
	notices := make([]FlightNotice, 0, maxFlightNotices)
	if finished(flight) || flight.Status == "diverted" {
		return notices, nil
	}
	departure, arrival := upcomingNoticeOperations(flight)
	groundStopDeparture := contextualGroundStopDeparture(flight)
	if departure == nil && arrival == nil && groundStopDeparture == nil {
		return notices, nil
	}
	cancellations, err := loadTFMSProgramCancellations(ctx, s.db, flight.Destination, flight.DestinationICAO, now)
	if err != nil {
		return nil, err
	}
	rows, err := currentFlightAdvisoryRows(ctx, s.db, flight, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var source, id string
		var observed, expires int64
		var raw []byte
		if err := rows.Scan(&source, &id, &observed, &expires, &raw); err != nil {
			return nil, err
		}
		var a feeds.Advisory
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		summary, decoded, ok := flightNoticeSummary(a)
		if !ok {
			continue
		}
		validUntil := time.Unix(0, expires).UTC()
		if a.Kind == "airport_closure" && decoded.FullAirportClosure && decoded.FixedValidity {
			// Show the published context and respect its expiry, even if the
			// normalized times conflict and prevent a flight prediction.
			a.StartsAt, a.EndsAt = decoded.StartsAt, decoded.EndsAt
		}
		for _, operation := range []struct {
			name, code, icao string
			at               *time.Time
		}{
			{"departure", flight.Origin, flight.OriginICAO, departure},
			{"arrival", flight.Destination, flight.DestinationICAO, arrival},
		} {
			if a.Airport != operation.code && a.Airport != operation.icao ||
				decoded.Airport != "" && decoded.Airport != operation.code && decoded.Airport != operation.icao ||
				a.Direction != "" && a.Direction != operation.name {
				continue
			}
			at := operation.at
			if operation.name == "arrival" && a.Kind == "ground_stop" {
				if a.AirportContextOnly {
					at = groundStopDeparture
				} else if at != nil {
					at = departure
				}
			}
			if at == nil {
				continue
			}
			occurrence, active := advisoryOccurrence(a, decoded, *at, now)
			if !active || !flightNoticeAppliesAt(occurrence, at, now) {
				continue
			}
			operationTime := *at
			if operationTime.Before(now) {
				operationTime = now
			}
			if a.IsTFMSAirportProgram() && latestTFMSProgramCancellation(cancellations, a.ID, operationTime).After(a.ObservedAt) {
				continue
			}
			deadline := validUntil
			if occurrence.EndsAt != nil {
				deadline = earliestTime(deadline, *occurrence.EndsAt)
			}
			notices = append(notices, FlightNotice{
				ID: hash(source + "\x00" + id + "\x00" + operation.name), Airport: a.Airport, Operation: operation.name,
				Kind: a.Kind, Source: source, Summary: summary, ObservedAt: time.Unix(0, observed).UTC(),
				ValidUntil: deadline, StartsAt: occurrence.StartsAt, EndsAt: occurrence.EndsAt,
			})
			if len(notices) == maxFlightNotices {
				return notices, rows.Close()
			}
		}
	}
	return notices, rows.Err()
}

type flightAdvisoryRowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func currentFlightAdvisoryRows(ctx context.Context, db flightAdvisoryRowsQueryer, flight feeds.Flight, now time.Time) (*sql.Rows, error) {
	// Compare revisions before checking freshness or applicability. An expired
	// or cancelled revision can supersede an earlier notice that still matches.
	t := now.UnixNano()
	return db.QueryContext(ctx, `WITH current_signed AS NOT MATERIALIZED (`+currentPushedAdvisories+`)
SELECT source,id,observed_ns,expires_ns,data FROM (
  SELECT p.source,p.id,p.observed_ns,p.expires_ns,p.data FROM polled_flight_notices p
  WHERE p.airport IN (?,?,?,?) AND p.airport<>'' AND p.expires_ns>? AND p.observed_ns<=?
    AND NOT EXISTS (SELECT 1 FROM current_signed a WHERE a.source=p.source AND a.id=p.id AND a.observed_ns>=p.observed_ns)
  UNION ALL
  SELECT a.source,a.id,a.observed_ns,a.expires_ns,a.data FROM current_signed a
  WHERE json_extract(a.data,'$.airport') IN (?,?,?,?) AND json_extract(a.data,'$.airport')<>''
    AND a.expires_ns>?
    AND NOT EXISTS (SELECT 1 FROM polled_flight_notices p WHERE p.source=a.source AND p.id=a.id
                    AND p.observed_ns>a.observed_ns AND p.observed_ns<=?)
) AS c WHERE NOT EXISTS (SELECT 1 FROM advisory_supersessions r WHERE r.source=c.source
  AND r.airport=json_extract(c.data,'$.airport') AND r.predecessor_id=c.id AND r.observed_ns BETWEEN c.observed_ns AND ?)
ORDER BY observed_ns DESC,source,id`,
		t, t, t,
		flight.Origin, flight.OriginICAO, flight.Destination, flight.DestinationICAO, t, t,
		flight.Origin, flight.OriginICAO, flight.Destination, flight.DestinationICAO, t, t, t)
}

func readCurrentFlightAdvisories(ctx context.Context, tx *sql.Tx, flight feeds.Flight, now time.Time) ([]feeds.Advisory, map[forecastAdvisory]time.Time, error) {
	rows, err := currentFlightAdvisoryRows(ctx, tx, flight, now)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var advisories []feeds.Advisory
	deadlines := make(map[forecastAdvisory]time.Time)
	for rows.Next() {
		var source, id string
		var observed, expires int64
		var raw []byte
		if err := rows.Scan(&source, &id, &observed, &expires, &raw); err != nil {
			return nil, nil, err
		}
		var advisory feeds.Advisory
		if err := json.Unmarshal(raw, &advisory); err != nil {
			return nil, nil, err
		}
		advisories = append(advisories, advisory)
		deadlines[advisoryEvidence(advisory)] = time.Unix(0, expires).UTC()
	}
	return advisories, deadlines, rows.Err()
}

func capCachedAdvisoryValidity(forecast *Prediction, deadlines map[forecastAdvisory]time.Time) {
	if forecast == nil {
		return
	}
	for _, evidence := range forecast.AdvisoryEvidence {
		if deadline, ok := deadlines[evidence]; ok {
			forecast.ValidUntil = earliestValidity(forecast.ValidUntil, deadline)
		}
	}
}

func upcomingNoticeOperations(flight feeds.Flight) (departure, arrival *time.Time) {
	if flight.ActualDeparture == nil && flight.ActualTakeoff == nil {
		switch flight.Status {
		case "taxi_out", "airborne", "landed":
		default:
			departure = effectiveDeparture(flight)
		}
	}
	if flight.ActualLanding == nil && flight.ActualArrival == nil && flight.Status != "landed" {
		arrival = effectiveArrival(flight)
	}
	return departure, arrival
}

func flightNoticeAppliesAt(a feeds.Advisory, at *time.Time, now time.Time) bool {
	if at == nil || at.Before(now.Add(-time.Hour)) {
		return false
	}
	operation := *at
	if operation.Before(now) {
		operation = now
	}
	end := a.ObservedAt.Add(feeds.AdvisoryMaxAge)
	if a.EndsAt != nil {
		end = *a.EndsAt
	}
	return (a.StartsAt == nil || !a.StartsAt.After(operation)) && end.After(operation)
}

func flightNoticeSummary(a feeds.Advisory) (summary string, decoded feeds.NOTAMSummary, ok bool) {
	if advisoryCancelled(a.Kind) {
		return "", decoded, false
	}
	summary = a.Reason
	decoded, recognized := feeds.DecodeNOTAM(a.RawReason)
	if recognized {
		if decoded.Scope == "cancellation" {
			return "", decoded, false
		}
		if summary == "" || summary == a.RawReason {
			summary = decoded.Text
		}
	} else if strings.TrimSpace(summary) == strings.TrimSpace(a.RawReason) {
		raw := strings.TrimSpace(a.RawReason)
		fields := strings.Fields(raw)
		if strings.HasPrefix(raw, "!") || len(fields) >= 2 &&
			(strings.EqualFold(fields[1], "NOTAMN") || strings.EqualFold(fields[1], "NOTAMR") || strings.EqualFold(fields[1], "NOTAMC")) {
			summary = ""
		}
	}
	summary = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, summary)), " ")
	if summary == "" {
		summary = strings.ReplaceAll(a.Kind, "_", " ")
	}
	return truncate(summary, 1024), decoded, true
}
