package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

// CompletedFlightCursor orders completed flights by SQLite's Julian arrival
// date and a namespaced flight identity. A zero arrival means no usable time.
type CompletedFlightCursor struct {
	Arrival float64
	ID      string
}

type CompletedFlightRecord struct {
	Flight feeds.Flight
	Cursor CompletedFlightCursor
}

// An older AeroAPI row can lack its alias record. Its ID can identify it only
// if a verified provider key elsewhere in the store matches exactly.
const completedFlightsSQL = `WITH owned AS MATERIALIZED (
  SELECT f.id,f.observed_ns,
    json_extract(f.snapshot,'$.status') AS status,
    json_extract(f.snapshot,'$.source') AS source,
    upper(trim(COALESCE(json_extract(f.snapshot,'$.origin'),''))) AS origin,
    upper(trim(COALESCE(json_extract(f.snapshot,'$.originICAO'),''))) AS origin_icao,
    upper(trim(COALESCE(json_extract(f.snapshot,'$.destination'),''))) AS destination,
    upper(trim(COALESCE(json_extract(f.snapshot,'$.destinationICAO'),''))) AS destination_icao,
    COALESCE(k.provider_id,CASE WHEN json_extract(f.snapshot,'$.source')='flightaware_aeroapi'
      AND EXISTS(SELECT 1 FROM aero_instance_keys known WHERE known.provider_id=f.id) THEN f.id END) AS provider_id
  FROM tracked t JOIN flights f ON f.id=t.flight_id
  LEFT JOIN aero_instance_keys k ON k.flight_id=f.id
  WHERE t.owner=?1
), ranked AS (
  SELECT owned.*,
    CASE WHEN provider_id IS NULL THEN 'l.'||id ELSE 'p.'||provider_id END AS cursor_id,
    ROW_NUMBER() OVER (
      PARTITION BY provider_id IS NULL,COALESCE(provider_id,id)
      ORDER BY observed_ns DESC,(id=provider_id) DESC,id
    ) AS alias_rank
  FROM owned WHERE status='arrived' OR (
    provider_id IS NOT NULL AND source='flightaware_aeroapi' AND status IN ('cancelled','diverted')
  )
), selected AS (
  SELECT r.id,r.observed_ns,r.cursor_id,(
    SELECT older.id FROM owned older
    WHERE older.provider_id=r.provider_id AND older.id<>r.id AND older.status='arrived'
      -- AeroAPI uses ICAO as the display code when the IATA code is absent.
      AND (older.origin='' OR r.origin='' OR older.origin=r.origin OR (
        r.origin_icao<>'' AND older.origin_icao=r.origin_icao AND r.origin_icao IN (older.origin,r.origin)
      ))
      AND (older.origin_icao='' OR r.origin_icao='' OR older.origin_icao=r.origin_icao)
      AND (older.destination='' OR r.destination='' OR older.destination=r.destination OR (
        r.destination_icao<>'' AND older.destination_icao=r.destination_icao AND r.destination_icao IN (older.destination,r.destination)
      ))
      AND (older.destination_icao='' OR r.destination_icao='' OR older.destination_icao=r.destination_icao)
    ORDER BY older.observed_ns DESC,(older.id=older.provider_id) DESC,older.id LIMIT 1
  ) AS older_id
  FROM ranked r WHERE r.alias_rank=1 AND r.status='arrived'
), timing AS (
  SELECT s.id,s.observed_ns,s.cursor_id,f.snapshot,p.snapshot AS older,
    julianday(json_extract(f.snapshot,'$.actualArrival')) AS actual_arrival,
    julianday(json_extract(f.snapshot,'$.scheduledArrival')) AS scheduled_arrival,
    julianday(json_extract(p.snapshot,'$.actualArrival')) AS older_actual,
    julianday(json_extract(p.snapshot,'$.scheduledArrival')) AS older_scheduled,
    julianday(json_extract(f.snapshot,'$.actualDeparture')) AS actual_departure,
    julianday(json_extract(f.snapshot,'$.actualTakeoff')) AS actual_takeoff,
    julianday(json_extract(f.snapshot,'$.actualLanding')) AS actual_landing,
    COALESCE(julianday(json_extract(f.snapshot,'$.scheduledDeparture')),
             julianday(json_extract(p.snapshot,'$.scheduledDeparture'))) AS scheduled_departure
  FROM selected s JOIN flights f ON f.id=s.id LEFT JOIN flights p ON p.id=s.older_id
), compatible AS (
  SELECT id,observed_ns,cursor_id,snapshot,older,actual_arrival,scheduled_arrival,
    CASE WHEN older_actual>COALESCE(actual_departure,0)
      AND older_actual>=max(COALESCE(actual_takeoff,0),COALESCE(actual_landing,0)) THEN older_actual END AS fallback_actual,
    CASE WHEN older_scheduled>COALESCE(scheduled_departure,0) THEN older_scheduled END AS fallback_scheduled
  FROM timing
), tracked_completed AS MATERIALIZED (
  SELECT id,observed_ns,cursor_id,snapshot,older,
    COALESCE(actual_arrival,fallback_actual,scheduled_arrival,fallback_scheduled,0) AS arrival,
    fallback_actual IS NOT NULL AS copy_actual,fallback_scheduled IS NOT NULL AS copy_scheduled
  FROM compatible
), tracked_departures AS MATERIALIZED (
  SELECT upper(trim(json_extract(snapshot,'$.number'))) AS number,
    upper(trim(json_extract(snapshot,'$.origin'))) AS origin,
    upper(trim(json_extract(snapshot,'$.originICAO'))) AS origin_icao,
    upper(trim(json_extract(snapshot,'$.destination'))) AS destination,
    upper(trim(json_extract(snapshot,'$.destinationICAO'))) AS destination_icao,
    json_extract(snapshot,'$.scheduledDeparture') AS departure
  FROM tracked_completed
  WHERE cursor_id GLOB 'p.*' AND json_extract(snapshot,'$.source')='flightaware_aeroapi'
    AND json_extract(snapshot,'$.scheduledDeparture') IS NOT NULL
), archive AS (
  SELECT id,snapshot,
    COALESCE(julianday(json_extract(snapshot,'$.actualArrival')),
             julianday(json_extract(snapshot,'$.scheduledArrival')),julianday(flight_date)) AS arrival,
    upper(trim(json_extract(snapshot,'$.number'))) AS number,
    upper(trim(COALESCE(NULLIF(json_extract(snapshot,'$.origin'),''),json_extract(snapshot,'$.originICAO')))) AS origin,
    upper(trim(COALESCE(NULLIF(json_extract(snapshot,'$.destination'),''),json_extract(snapshot,'$.destinationICAO')))) AS destination,
    json_extract(snapshot,'$.scheduledDeparture') AS departure
  FROM passport_archive WHERE owner=?1
), completed AS (
  SELECT * FROM tracked_completed
  UNION ALL
  SELECT a.id,0,'a.'||a.id,a.snapshot,NULL,a.arrival,0,0 FROM archive a
  -- Offset-free CSV times carry no comparable departure instant. Keep those
  -- rows, even when the flight number and route match a tracked flight.
  WHERE a.departure IS NULL OR a.number='' OR NOT EXISTS (
    SELECT 1 FROM tracked_departures t
    WHERE t.number=a.number AND a.origin IN (t.origin,t.origin_icao)
      AND a.destination IN (t.destination,t.destination_icao)
      AND (substr(t.departure,-1)='Z' OR (
        substr(t.departure,-6,1) IN ('+','-') AND substr(t.departure,-3,1)=':'
      ))
      AND (t.departure=a.departure OR (
        instr(t.departure,'.')=0 AND instr(a.departure,'.')=0
        AND unixepoch(t.departure)=unixepoch(a.departure)
      ))
  )
)
`

const completedFlightRowsSQL = completedFlightsSQL + `SELECT c.snapshot,c.arrival,c.cursor_id,c.older,c.copy_actual,c.copy_scheduled FROM completed c`

// CompletedFlights returns the installation's full Passport history,
// independently of the tracked-flight dashboard's row limit.
func (s *Store) CompletedFlights(ctx context.Context, owner string) ([]feeds.Flight, error) {
	flights := make([]feeds.Flight, 0)
	err := s.walkCompletedFlightRows(ctx, completedFlightRowsSQL+` ORDER BY c.arrival DESC,c.observed_ns DESC,c.id`, []any{owner}, func(flight feeds.Flight, _ CompletedFlightCursor) {
		flights = append(flights, flight)
	})
	if err != nil {
		return nil, err
	}
	return flights, nil
}

// WalkCompletedFlights streams the full history in the same order as the paged
// history. The visitor must finish processing each row without querying Store.
func (s *Store) WalkCompletedFlights(ctx context.Context, owner string, visit func(feeds.Flight, CompletedFlightCursor)) error {
	return s.walkCompletedFlightRows(ctx, completedFlightRowsSQL+` ORDER BY c.arrival DESC,c.cursor_id`, []any{owner}, visit)
}

// CompletedFlightPage returns a bounded slice and the key of its last row if
// there are more rows. Continuations always reapply installation ownership.
func (s *Store) CompletedFlightPage(ctx context.Context, owner string, after *CompletedFlightCursor, limit int) ([]CompletedFlightRecord, *CompletedFlightCursor, error) {
	if limit < 1 || limit > 100 {
		return nil, nil, errors.New("invalid completed-flight page size")
	}
	query := completedFlightRowsSQL
	args := []any{owner}
	if after != nil {
		query += ` WHERE c.arrival<? OR (c.arrival=? AND c.cursor_id>?)`
		args = append(args, after.Arrival, after.Arrival, after.ID)
	}
	query += ` ORDER BY c.arrival DESC,c.cursor_id LIMIT ?`
	args = append(args, limit+1)
	flights := make([]CompletedFlightRecord, 0, limit)
	var next *CompletedFlightCursor
	err := s.walkCompletedFlightRows(ctx, query, args, func(flight feeds.Flight, cursor CompletedFlightCursor) {
		if len(flights) == limit {
			next = &flights[limit-1].Cursor
			return
		}
		flights = append(flights, CompletedFlightRecord{Flight: flight, Cursor: cursor})
	})
	if err != nil {
		return nil, nil, err
	}
	return flights, next, nil
}

func (s *Store) walkCompletedFlightRows(ctx context.Context, query string, args []any, visit func(feeds.Flight, CompletedFlightCursor)) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw, previous []byte
		var cursor CompletedFlightCursor
		var copyActual, copyScheduled bool
		if err := rows.Scan(&raw, &cursor.Arrival, &cursor.ID, &previous, &copyActual, &copyScheduled); err != nil {
			return err
		}
		var flight feeds.Flight
		if err := json.Unmarshal(raw, &flight); err != nil {
			return err
		}
		if len(previous) != 0 {
			var older feeds.Flight
			if err := json.Unmarshal(previous, &older); err != nil {
				return err
			}
			fillPassportMeasurements(&flight, older, copyActual, copyScheduled)
		}
		visit(flight, cursor)
	}
	return rows.Err()
}

func fillPassportMeasurements(flight *feeds.Flight, older feeds.Flight, copyActual, copyScheduled bool) {
	flight.Origin = cmp.Or(flight.Origin, older.Origin)
	flight.OriginICAO = cmp.Or(flight.OriginICAO, older.OriginICAO)
	flight.Destination = cmp.Or(flight.Destination, older.Destination)
	flight.DestinationICAO = cmp.Or(flight.DestinationICAO, older.DestinationICAO)
	if flight.ActualArrival == nil && copyActual {
		flight.ActualArrival = older.ActualArrival
	}
	if flight.ScheduledArrival == nil && copyScheduled {
		flight.ScheduledArrival = older.ScheduledArrival
	}
	if flight.ActualDeparture == nil && older.ActualDeparture != nil && !older.ActualDeparture.IsZero() &&
		beforeOrMissing(*older.ActualDeparture, flight.ActualTakeoff) && beforeOrMissing(*older.ActualDeparture, flight.ActualLanding) &&
		beforeOrMissing(*older.ActualDeparture, flight.ActualArrival) {
		flight.ActualDeparture = older.ActualDeparture
	}
	if flight.DistanceMiles == nil && older.DistanceMiles != nil && *older.DistanceMiles > 0 {
		flight.DistanceMiles = older.DistanceMiles
	}
	flight.TailNumber = cmp.Or(flight.TailNumber, older.TailNumber)
}

func beforeOrMissing(at time.Time, later *time.Time) bool {
	return later == nil || later.IsZero() || at.Before(*later)
}
