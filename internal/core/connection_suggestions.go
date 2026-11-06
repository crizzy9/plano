package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const connectionSuggestionLegLimit = 128

// ScheduleConnectionWindow is the longest suggested layover without booking evidence.
const ScheduleConnectionWindow = 8 * time.Hour

// ConnectionSuggestionFlight holds a tracked snapshot and installation-scoped
// relationships. The booking index is resolved in SQLite without returning digests.
type ConnectionSuggestionFlight struct {
	Flight          feeds.Flight
	BookedOnward    []string
	MonitoredOnward []string
}

// ConnectionSuggestionFlights reads a bounded pool and its relationships in
// one snapshot so concurrent untracks cannot leave a pair with a missing leg.
func (s *Store) ConnectionSuggestionFlights(ctx context.Context, owner string, now, latest time.Time) ([]ConnectionSuggestionFlight, error) {
	// Narrow arrivals against a bounded set of usable onward legs before
	// applying the arrival limit. Round Julian differences to milliseconds
	// to preserve boundary values at SQLite's timestamp resolution. The
	// assessor checks the exact times and status.
	rows, err := s.db.QueryContext(ctx, `
WITH owner_flights AS MATERIALIZED (
  SELECT f.id,
    julianday(json_extract(f.snapshot, '$.scheduledArrival')) AS arrival,
    julianday(json_extract(f.snapshot, '$.scheduledDeparture')) AS scheduled_departure,
    julianday(COALESCE(json_extract(f.snapshot, '$.estimatedDeparture'), json_extract(f.snapshot, '$.scheduledDeparture'))) AS departure,
    coalesce(json_extract(f.snapshot, '$.origin'), '') AS origin,
    coalesce(json_extract(f.snapshot, '$.originICAO'), '') AS origin_icao,
    coalesce(json_extract(f.snapshot, '$.destination'), '') AS destination,
    coalesce(json_extract(f.snapshot, '$.destinationICAO'), '') AS destination_icao,
    coalesce(json_extract(f.snapshot, '$.status'), '') AS status,
    coalesce(json_extract(f.snapshot, '$.actualDeparture'), json_extract(f.snapshot, '$.actualTakeoff'),
      json_extract(f.snapshot, '$.actualLanding'), json_extract(f.snapshot, '$.actualArrival')) IS NOT NULL AS departed
  FROM tracked t JOIN flights f ON f.id=t.flight_id
  WHERE t.owner=:owner
), onward AS MATERIALIZED (
  SELECT id, scheduled_departure, origin, origin_icao FROM owner_flights
  WHERE scheduled_departure IS NOT NULL
    AND departure BETWEEN julianday(:now) AND julianday(:latest)
    AND (origin<>'' OR origin_icao<>'')
    AND NOT departed AND status NOT IN ('cancelled','taxi_out','airborne','landed','arrived','diverted')
  ORDER BY departure, id
  LIMIT :limit
), inbound AS MATERIALIZED (
  SELECT f.id FROM owner_flights f
  WHERE f.status NOT IN ('cancelled','diverted')
    AND EXISTS (
      SELECT 1 FROM onward o
      WHERE o.id<>f.id AND f.arrival BETWEEN o.scheduled_departure-1 AND o.scheduled_departure
        AND CASE WHEN f.destination_icao<>'' AND o.origin_icao<>'' THEN f.destination_icao=o.origin_icao
          ELSE f.destination<>'' AND f.destination=o.origin END
        AND (round((o.scheduled_departure-f.arrival)*86400,3)<=:schedule_window OR EXISTS (
          SELECT 1 FROM bookings b JOIN bookings next
            ON next.owner=b.owner AND next.flight_id=o.id AND next.locator_hash=b.locator_hash
          WHERE b.owner=:owner AND b.flight_id=f.id
        ))
    )
  ORDER BY f.arrival, f.id
  LIMIT :limit
), candidates AS MATERIALIZED (
  SELECT id, arrival, scheduled_departure FROM owner_flights
  WHERE id IN (SELECT id FROM inbound UNION SELECT id FROM onward)
)
SELECT f.snapshot,
  (SELECT json_group_array(o.id) FROM candidates o
    WHERE o.scheduled_departure BETWEEN c.arrival AND c.arrival+1 AND o.id<>c.id
    AND EXISTS (
      SELECT 1 FROM bookings b JOIN bookings next
        ON next.owner=b.owner AND next.flight_id=o.id AND next.locator_hash=b.locator_hash
      WHERE b.owner=:owner AND b.flight_id=c.id
    )),
  (SELECT json_group_array(outbound) FROM connection_monitors
    WHERE owner=:owner AND inbound=c.id AND retire_ns=0)
FROM candidates c JOIN flights f ON f.id=c.id`,
		sql.Named("owner", owner), sql.Named("now", now.Format(time.RFC3339Nano)),
		sql.Named("latest", latest.Format(time.RFC3339Nano)), sql.Named("limit", connectionSuggestionLegLimit),
		sql.Named("schedule_window", int(ScheduleConnectionWindow/time.Second)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []ConnectionSuggestionFlight
	for rows.Next() {
		var candidate ConnectionSuggestionFlight
		var snapshot, booked, monitored []byte
		if err := rows.Scan(&snapshot, &booked, &monitored); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(snapshot, &candidate.Flight); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(booked, &candidate.BookedOnward); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(monitored, &candidate.MonitoredOnward); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}
