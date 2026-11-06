package core

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode"

	"github.com/crizzy9/plano/internal/feeds"
)

const (
	maxFlightNotices   = 8
	maxPolledNotices   = 4096
	maxNoticeAge       = 6 * time.Hour
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
	for _, a := range advisories {
		if a.Source != source || a.ID == "" || a.ObservedAt.IsZero() {
			continue
		}
		expires := a.ObservedAt.Add(maxNoticeAge)
		if pollExpiry := polledAt.Add(polledNoticeMaxAge); pollExpiry.Before(expires) {
			expires = pollExpiry
		}
		if a.EndsAt != nil && a.EndsAt.Before(expires) {
			expires = *a.EndsAt
		}
		entries = append(entries, entry{a, a.ObservedAt.UnixNano(), expires.UnixNano()})
		if len(entries) == maxPolledNotices {
			break
		}
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
	if departure == nil && arrival == nil {
		return notices, nil
	}
	// Compare revisions before checking freshness or applicability. An expired
	// or cancelled revision can supersede an earlier notice that still matches.
	rows, err := s.db.QueryContext(ctx, `SELECT source,id,observed_ns,expires_ns,data FROM (
  SELECT p.source,p.id,p.observed_ns,p.expires_ns,p.data FROM polled_flight_notices p
  WHERE p.airport IN (?,?,?,?) AND p.airport<>'' AND p.expires_ns>? AND p.observed_ns<=?
    AND NOT EXISTS (SELECT 1 FROM pushed_advisories a WHERE a.source=p.source AND a.id=p.id
                    AND a.observed_ns>=p.observed_ns AND a.observed_ns<=?)
  UNION ALL
  SELECT a.source,a.id,a.observed_ns,a.expires_ns,a.data FROM pushed_advisories a
  WHERE json_extract(a.data,'$.airport') IN (?,?,?,?) AND json_extract(a.data,'$.airport')<>''
    AND a.expires_ns>? AND a.observed_ns<=?
    AND NOT EXISTS (SELECT 1 FROM polled_flight_notices p WHERE p.source=a.source AND p.id=a.id
                    AND p.observed_ns>a.observed_ns AND p.observed_ns<=?)
) ORDER BY observed_ns DESC,source,id`,
		flight.Origin, flight.OriginICAO, flight.Destination, flight.DestinationICAO, now.UnixNano(), now.UnixNano(), now.UnixNano(),
		flight.Origin, flight.OriginICAO, flight.Destination, flight.DestinationICAO, now.UnixNano(), now.UnixNano(), now.UnixNano())
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
		summary, decodedAirport, ok := flightNoticeSummary(a)
		if !ok {
			continue
		}
		for _, operation := range []struct {
			name, code, icao string
			at               *time.Time
		}{
			{"departure", flight.Origin, flight.OriginICAO, departure},
			{"arrival", flight.Destination, flight.DestinationICAO, arrival},
		} {
			if operation.at == nil || a.Airport != operation.code && a.Airport != operation.icao ||
				decodedAirport != "" && decodedAirport != operation.code && decodedAirport != operation.icao ||
				a.Direction != "" && a.Direction != operation.name {
				continue
			}
			at := operation.at
			if operation.name == "arrival" && a.Kind == "ground_stop" {
				at = departure
			}
			if !flightNoticeAppliesAt(a, at, now) {
				continue
			}
			notices = append(notices, FlightNotice{
				ID: hash(source + "\x00" + id + "\x00" + operation.name), Airport: a.Airport, Operation: operation.name,
				Kind: a.Kind, Source: source, Summary: summary, ObservedAt: time.Unix(0, observed).UTC(),
				ValidUntil: time.Unix(0, expires).UTC(), StartsAt: a.StartsAt, EndsAt: a.EndsAt,
			})
			if len(notices) == maxFlightNotices {
				return notices, rows.Close()
			}
		}
	}
	return notices, rows.Err()
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
	end := a.ObservedAt.Add(maxNoticeAge)
	if a.EndsAt != nil {
		end = *a.EndsAt
	}
	return (a.StartsAt == nil || !a.StartsAt.After(operation)) && end.After(operation)
}

func flightNoticeSummary(a feeds.Advisory) (summary, airport string, ok bool) {
	switch a.Kind {
	case "cancelled", "canceled", "cancellation", "notam_cancelled", "notam_canceled", "notam_cancellation":
		return "", "", false
	}
	summary = a.Reason
	if decoded, ok := feeds.DecodeNOTAM(a.RawReason); ok {
		if decoded.Scope == "cancellation" {
			return "", "", false
		}
		airport = decoded.Airport
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
	return truncate(summary, 1024), airport, true
}
