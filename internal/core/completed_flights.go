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

const passportArchiveMatchesTracked = `a.number IN (t.number,t.icao_number)
      AND a.origin IN (t.origin,t.origin_icao) AND a.destination IN (t.destination,t.destination_icao)
      AND (substr(t.departure,-1)='Z' OR (
        substr(t.departure,-6,1) IN ('+','-') AND substr(t.departure,-3,1)=':'
      ))
      AND (t.departure=a.departure OR (
        instr(t.departure,'.')=0 AND instr(a.departure,'.')=0
        AND unixepoch(t.departure)=unixepoch(a.departure)
      ))`

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
    julianday(json_extract(p.snapshot,'$.actualDeparture')) AS older_departure,
    julianday(json_extract(f.snapshot,'$.actualTakeoff')) AS actual_takeoff,
    julianday(json_extract(f.snapshot,'$.actualLanding')) AS actual_landing,
    COALESCE(julianday(json_extract(f.snapshot,'$.scheduledDeparture')),
             julianday(json_extract(p.snapshot,'$.scheduledDeparture'))) AS scheduled_departure
  FROM selected s JOIN flights f ON f.id=s.id LEFT JOIN flights p ON p.id=s.older_id
), compatible AS (
  SELECT id,observed_ns,cursor_id,snapshot,older,actual_arrival,scheduled_arrival,older_departure,
    actual_takeoff,actual_landing,scheduled_departure,
    CASE WHEN older_actual>COALESCE(actual_departure,0)
      AND older_actual>=max(COALESCE(actual_takeoff,0),COALESCE(actual_landing,0)) THEN older_actual END AS fallback_actual,
    CASE WHEN older_scheduled>COALESCE(scheduled_departure,0) THEN older_scheduled END AS fallback_scheduled
  FROM timing
), tracked_departures AS MATERIALIZED (
  SELECT id,upper(trim(json_extract(snapshot,'$.number'))) AS number,
    upper(trim(json_extract(snapshot,'$.icaoNumber'))) AS icao_number,
    upper(trim(json_extract(snapshot,'$.origin'))) AS origin,
    upper(trim(json_extract(snapshot,'$.originICAO'))) AS origin_icao,
    upper(trim(json_extract(snapshot,'$.destination'))) AS destination,
    upper(trim(json_extract(snapshot,'$.destinationICAO'))) AS destination_icao,
    json_extract(snapshot,'$.scheduledDeparture') AS departure
  FROM compatible
  WHERE cursor_id GLOB 'p.*' AND json_extract(snapshot,'$.source')='flightaware_aeroapi'
    AND json_extract(snapshot,'$.scheduledDeparture') IS NOT NULL
), archive AS MATERIALIZED (
  SELECT id,snapshot,
    COALESCE(julianday(json_extract(snapshot,'$.actualArrival')),
             julianday(json_extract(snapshot,'$.scheduledArrival')),julianday(flight_date)) AS arrival,
    upper(trim(json_extract(snapshot,'$.number'))) AS number,
    upper(trim(COALESCE(NULLIF(json_extract(snapshot,'$.origin'),''),json_extract(snapshot,'$.originICAO')))) AS origin,
    upper(trim(COALESCE(NULLIF(json_extract(snapshot,'$.destination'),''),json_extract(snapshot,'$.destinationICAO')))) AS destination,
    json_extract(snapshot,'$.scheduledDeparture') AS departure
  FROM passport_archive WHERE owner=?1
), archive_overlap AS MATERIALIZED (
  -- Preserve suppression for every exact match, but retain an ID only when
  -- the archive row identifies one tracked flight. Two candidates suffice.
  SELECT a.id,(
    SELECT CASE count(*) WHEN 1 THEN min(id) WHEN 2 THEN '' END
    FROM (SELECT t.id FROM tracked_departures t WHERE ` + passportArchiveMatchesTracked + ` LIMIT 2)
  ) AS tracked_id
  FROM archive a WHERE a.departure IS NOT NULL AND a.number<>''
), archive_fallback AS (
  SELECT t.id,a.snapshot
  FROM tracked_departures t JOIN archive_overlap m ON m.tracked_id=t.id JOIN archive a ON a.id=m.id
  WHERE json_extract(a.snapshot,'$.source')='flighty_csv' AND 1=(
    SELECT count(*) FROM (SELECT 1 FROM archive a WHERE ` + passportArchiveMatchesTracked + ` LIMIT 2)
  )
), alias_departure AS (
  SELECT *,CASE WHEN older_departure<>julianday('0001-01-01')
    AND older_departure<min(
      COALESCE(NULLIF(actual_takeoff,julianday('0001-01-01')),older_departure+1),
      COALESCE(NULLIF(actual_landing,julianday('0001-01-01')),older_departure+1),
      COALESCE(NULLIF(COALESCE(actual_arrival,fallback_actual),julianday('0001-01-01')),older_departure+1)
    )
    THEN json_extract(older,'$.actualDeparture') END AS fallback_gate_out
  FROM compatible
), archive_timing AS (
  SELECT c.*,a.snapshot AS archived,
    julianday(json_extract(a.snapshot,'$.actualArrival')) AS archive_actual,
    julianday(json_extract(a.snapshot,'$.scheduledArrival')) AS archive_scheduled,
    json_extract(a.snapshot,'$.actualDeparture') AS archive_gate_out,
    julianday(json_extract(a.snapshot,'$.actualDeparture')) AS archive_departure,
    COALESCE(json_extract(c.snapshot,'$.actualDeparture'),fallback_gate_out) AS known_gate_out
  FROM alias_departure c LEFT JOIN archive_fallback a ON a.id=c.id
), archive_compatible AS (
  SELECT *,
    CASE WHEN archive_actual>COALESCE(julianday(known_gate_out),archive_departure,0)
      AND archive_actual>=max(COALESCE(actual_takeoff,0),COALESCE(actual_landing,0))
      AND archive_actual<=COALESCE(julianday(known_gate_out),archive_departure,archive_actual)+2
      AND (archive_departure IS NULL OR ((actual_takeoff IS NULL OR archive_departure<actual_takeoff)
        AND (actual_landing IS NULL OR archive_departure<actual_landing)))
      AND (known_gate_out IS NULL OR archive_gate_out IS NULL OR known_gate_out=archive_gate_out OR (
        instr(known_gate_out,'.')=0 AND instr(archive_gate_out,'.')=0
        AND unixepoch(known_gate_out)=unixepoch(archive_gate_out)
      )) THEN archive_actual END AS archive_fallback_actual,
    CASE WHEN archive_scheduled>scheduled_departure AND archive_scheduled<=scheduled_departure+2
      THEN archive_scheduled END AS archive_fallback_scheduled
  FROM archive_timing
), completed AS (
  SELECT id,observed_ns,cursor_id,snapshot,older,archived,
    COALESCE(actual_arrival,fallback_actual,archive_fallback_actual,scheduled_arrival,fallback_scheduled,archive_fallback_scheduled,0) AS arrival,
    fallback_actual IS NOT NULL AS copy_actual,fallback_scheduled IS NOT NULL AS copy_scheduled,
    fallback_gate_out IS NOT NULL AS copy_gate_out,
    archive_fallback_actual IS NOT NULL AS copy_archive_actual,archive_fallback_scheduled IS NOT NULL AS copy_archive_scheduled
  FROM archive_compatible
  UNION ALL
  SELECT a.id,0,'a.'||a.id,a.snapshot,NULL,NULL,a.arrival,0,0,0,0,0
  FROM archive a LEFT JOIN archive_overlap m ON m.id=a.id
  WHERE m.tracked_id IS NULL
)
`

const completedFlightRowsSQL = completedFlightsSQL + `SELECT c.snapshot,c.arrival,c.cursor_id,c.older,c.copy_actual,c.copy_scheduled,c.copy_gate_out,
c.archived,c.copy_archive_actual,c.copy_archive_scheduled FROM completed c`

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
		var raw, previous, archived []byte
		var cursor CompletedFlightCursor
		var copyActual, copyScheduled, copyGateOut, copyArchiveActual, copyArchiveScheduled bool
		if err := rows.Scan(&raw, &cursor.Arrival, &cursor.ID, &previous, &copyActual, &copyScheduled, &copyGateOut,
			&archived, &copyArchiveActual, &copyArchiveScheduled); err != nil {
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
			// Without an archive match, keep the original full-precision
			// alias gate-out check. Otherwise use the query's shared gate.
			fillPassportMeasurements(&flight, older, copyActual, copyScheduled, copyGateOut || len(archived) == 0)
		}
		if len(archived) != 0 {
			var archive feeds.Flight
			if err := json.Unmarshal(archived, &archive); err != nil {
				return err
			}
			fillPassportArchiveMeasurements(&flight, archive, copyArchiveActual, copyArchiveScheduled)
		}
		visit(flight, cursor)
	}
	return rows.Err()
}

func fillPassportMeasurements(flight *feeds.Flight, older feeds.Flight, copyActual, copyScheduled, copyGateOut bool) {
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
	if flight.ActualDeparture == nil && copyGateOut && older.ActualDeparture != nil && !older.ActualDeparture.IsZero() &&
		beforeOrMissing(*older.ActualDeparture, flight.ActualTakeoff) && beforeOrMissing(*older.ActualDeparture, flight.ActualLanding) &&
		beforeOrMissing(*older.ActualDeparture, flight.ActualArrival) {
		flight.ActualDeparture = older.ActualDeparture
	}
	if flight.DistanceMiles == nil && older.DistanceMiles != nil && *older.DistanceMiles > 0 {
		flight.DistanceMiles = older.DistanceMiles
	}
	flight.TailNumber = cmp.Or(flight.TailNumber, older.TailNumber)
}

func fillPassportArchiveMeasurements(flight *feeds.Flight, archive feeds.Flight, copyActual, copyScheduled bool) {
	flight.HistoryDate = cmp.Or(flight.HistoryDate, archive.HistoryDate)
	flight.TailNumber = cmp.Or(flight.TailNumber, archive.TailNumber)
	if flight.ScheduledArrival == nil && copyScheduled {
		flight.ScheduledArrival = archive.ScheduledArrival
	}
	if flight.ActualArrival == nil && copyActual {
		flight.ActualArrival = archive.ActualArrival
	}
	if archive.ActualArrival != nil && (flight.ActualArrival == nil || passportTimeConflicts(flight.ActualArrival, archive.ActualArrival)) {
		// Do not pair a known gate-out with a rejected archive gate-in.
		return
	}
	if flight.ActualDeparture == nil && archive.ActualDeparture != nil && !archive.ActualDeparture.IsZero() &&
		beforeOrMissing(*archive.ActualDeparture, flight.ActualTakeoff) && beforeOrMissing(*archive.ActualDeparture, flight.ActualLanding) &&
		beforeOrMissing(*archive.ActualDeparture, flight.ActualArrival) &&
		(flight.ActualArrival == nil || flight.ActualArrival.Sub(*archive.ActualDeparture) <= 48*time.Hour) {
		flight.ActualDeparture = archive.ActualDeparture
	}
}

func beforeOrMissing(at time.Time, later *time.Time) bool {
	return later == nil || later.IsZero() || at.Before(*later)
}
