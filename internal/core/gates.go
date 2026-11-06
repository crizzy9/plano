package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

type GatePrediction struct {
	Gate    string  `json:"gate"`
	Samples int     `json:"samples"`
	Share   float64 `json:"share"`
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const gateHistorySQL = `WITH target(provider_id) AS (
  SELECT COALESCE(k.provider_id,CASE WHEN json_extract(f.snapshot,'$.source')='flightaware_aeroapi' THEN f.id END)
  FROM flights f LEFT JOIN aero_instance_keys k ON k.flight_id=f.id WHERE f.id=@id
), scoped AS MATERIALIZED (
  SELECT f.id,f.observed_ns,
    json_extract(f.snapshot,'$.status') AS status,
    upper(trim(json_extract(f.snapshot,@gate),char(9,10,11,12,13,32))) AS gate,
    upper(trim(COALESCE(json_extract(f.snapshot,@terminalPath),''),char(9,10,11,12,13,32))) AS terminal,
    upper(trim(COALESCE(json_extract(f.snapshot,'$.originICAO'),''))) AS origin_icao,
    upper(trim(COALESCE(json_extract(f.snapshot,'$.destinationICAO'),''))) AS destination_icao,
    julianday(json_extract(f.snapshot,@actual)) AS actual,
    COALESCE(k.provider_id,CASE WHEN json_extract(f.snapshot,'$.source')='flightaware_aeroapi'
      AND EXISTS(SELECT 1 FROM aero_instance_keys known WHERE known.provider_id=f.id) THEN f.id END) AS provider_id
  FROM flights f LEFT JOIN aero_instance_keys k ON k.flight_id=f.id
  WHERE f.id<>@id AND f.observed_ns<=@asOf AND json_extract(f.snapshot,'$.number')=@number
    AND json_extract(f.snapshot,'$.origin')=@origin AND json_extract(f.snapshot,'$.destination')=@destination
), ranked AS (
  SELECT scoped.*,ROW_NUMBER() OVER (
    PARTITION BY provider_id IS NULL,COALESCE(provider_id,id)
    ORDER BY observed_ns DESC,(id=provider_id) DESC,id
  ) AS alias_rank
  FROM scoped
  WHERE actual IS NOT NULL AND status NOT IN ('cancelled','diverted')
), samples AS MATERIALIZED (
  SELECT * FROM ranked r
  WHERE r.alias_rank=1 AND r.gate<>''
    AND r.actual BETWEEN julianday(@from) AND julianday(@through)
    AND (@terminal='' OR r.terminal=@terminal)
    AND (r.provider_id IS NULL OR r.provider_id<>COALESCE((SELECT provider_id FROM target),NULLIF(@provider,''),''))
), linked(provider_id,flight_id) AS (
  SELECT k.provider_id,k.flight_id FROM samples s JOIN aero_instance_keys k ON k.provider_id=s.provider_id
  UNION ALL
  -- Older canonical rows can lack an index entry of their own.
  SELECT s.provider_id,s.provider_id FROM samples s WHERE s.provider_id IS NOT NULL
    AND NOT EXISTS(SELECT 1 FROM aero_instance_keys k WHERE k.flight_id=s.provider_id)
), authority AS MATERIALIZED (
  SELECT l.provider_id,f.id,f.observed_ns,json_extract(f.snapshot,'$.status') AS status,
    upper(trim(COALESCE(json_extract(f.snapshot,@gate),''),char(9,10,11,12,13,32))) AS gate,
    upper(trim(COALESCE(json_extract(f.snapshot,@terminalPath),''),char(9,10,11,12,13,32))) AS terminal,
    upper(trim(COALESCE(json_extract(f.snapshot,'$.origin'),''))) AS origin,
    upper(trim(COALESCE(json_extract(f.snapshot,'$.originICAO'),''))) AS origin_icao,
    upper(trim(COALESCE(json_extract(f.snapshot,'$.destination'),''))) AS destination,
    upper(trim(COALESCE(json_extract(f.snapshot,'$.destinationICAO'),''))) AS destination_icao
  FROM linked l JOIN flights f ON f.id=l.flight_id
  WHERE f.observed_ns<=@asOf AND json_extract(f.snapshot,'$.source')='flightaware_aeroapi'
), counts AS (
  SELECT s.gate,COUNT(*) AS n FROM samples s
  -- Check verified corrections outside the sample route and allow them to
  -- disqualify older gate evidence even without another operation timestamp.
  WHERE NOT EXISTS(SELECT 1 FROM authority c WHERE c.provider_id=s.provider_id
    AND (c.observed_ns>s.observed_ns OR (c.observed_ns=s.observed_ns AND
      ((c.id=c.provider_id)>(s.id=s.provider_id) OR
        (c.id=c.provider_id)=(s.id=s.provider_id) AND c.id<s.id)))
    AND (c.status IN ('cancelled','diverted') OR c.gate<>'' AND c.gate<>s.gate
      OR c.terminal<>'' AND c.terminal<>s.terminal
      OR CASE WHEN c.origin_icao<>'' AND s.origin_icao<>'' THEN c.origin_icao<>s.origin_icao
        ELSE c.origin<>'' AND c.origin NOT IN (upper(trim(@origin)),s.origin_icao) AND c.origin_icao<>upper(trim(@origin)) END
      OR CASE WHEN c.destination_icao<>'' AND s.destination_icao<>'' THEN c.destination_icao<>s.destination_icao
        ELSE c.destination<>'' AND c.destination NOT IN (upper(trim(@destination)),s.destination_icao)
          AND c.destination_icao<>upper(trim(@destination)) END))
  GROUP BY s.gate
)
SELECT gate,(SELECT SUM(n) FROM counts),n*1.0/(SELECT SUM(n) FROM counts)
FROM counts ORDER BY n DESC,gate LIMIT 1`

// PredictGate summarizes actual gate observations for the same route and
// designator, counting each verified flight once and using the reported
// terminal when known. Assigned gates always take precedence over history.
func (s *Store) PredictGate(ctx context.Context, flight feeds.Flight, arrival bool, now time.Time) (*GatePrediction, error) {
	return predictGate(ctx, s.db, flight, arrival, now)
}

func predictGate(ctx context.Context, db rowQueryer, flight feeds.Flight, arrival bool, now time.Time) (*GatePrediction, error) {
	if finished(flight) || flight.Status == "diverted" || flight.Number == "" || flight.Origin == "" || flight.Destination == "" ||
		arrival && flight.ArrivalGate != "" || !arrival && flight.DepartureGate != "" {
		return nil, nil
	}
	gate, actual, terminalPath, terminal := "$.departureGate", "$.actualDeparture", "$.departureTerminal", flight.DepartureTerminal
	if arrival {
		gate, actual, terminalPath, terminal = "$.arrivalGate", "$.actualArrival", "$.arrivalTerminal", flight.ArrivalTerminal
	}
	terminal = strings.ToUpper(strings.TrimSpace(terminal))
	var prediction GatePrediction
	err := db.QueryRowContext(ctx, gateHistorySQL,
		sql.Named("gate", gate), sql.Named("actual", actual), sql.Named("terminalPath", terminalPath),
		sql.Named("id", flight.ID), sql.Named("provider", flight.AeroProviderID()), sql.Named("number", flight.Number),
		sql.Named("origin", flight.Origin), sql.Named("destination", flight.Destination), sql.Named("terminal", terminal),
		sql.Named("asOf", now.UnixNano()), sql.Named("from", now.AddDate(0, 0, -90).Format(time.RFC3339Nano)),
		sql.Named("through", now.Format(time.RFC3339Nano)),
	).Scan(&prediction.Gate, &prediction.Samples, &prediction.Share)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (prediction.Samples < 3 || prediction.Share < 0.60) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &prediction, nil
}
