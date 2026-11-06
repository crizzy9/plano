package core

import (
	"context"
	"database/sql"
	"errors"
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

// PredictGate summarizes actual gate observations for the same route and
// designator. Assigned gates always take precedence over the history.
func (s *Store) PredictGate(ctx context.Context, flight feeds.Flight, arrival bool, now time.Time) (*GatePrediction, error) {
	return predictGate(ctx, s.db, flight, arrival, now)
}

func predictGate(ctx context.Context, db rowQueryer, flight feeds.Flight, arrival bool, now time.Time) (*GatePrediction, error) {
	if finished(flight) || flight.Number == "" || flight.Origin == "" || flight.Destination == "" || arrival && flight.ArrivalGate != "" || !arrival && flight.DepartureGate != "" {
		return nil, nil
	}
	gate, actual := "$.departureGate", "$.actualDeparture"
	if arrival {
		gate, actual = "$.arrivalGate", "$.actualArrival"
	}
	var prediction GatePrediction
	err := db.QueryRowContext(ctx, `WITH past(gate) AS (
 SELECT json_extract(snapshot,?) FROM flights
 WHERE id <> ? AND json_extract(snapshot,'$.number') = ? AND json_extract(snapshot,'$.origin') = ? AND json_extract(snapshot,'$.destination') = ?
 AND json_extract(snapshot,'$.status') NOT IN ('cancelled','diverted')
 AND julianday(json_extract(snapshot,?)) BETWEEN julianday(?) AND julianday(?)
), counts AS (SELECT gate,COUNT(*) AS n FROM past WHERE gate<>'' GROUP BY gate)
SELECT gate,(SELECT SUM(n) FROM counts),n*1.0/(SELECT SUM(n) FROM counts) FROM counts ORDER BY n DESC,gate LIMIT 1`, gate, flight.ID, flight.Number, flight.Origin, flight.Destination, actual, now.AddDate(0, 0, -90).Format(time.RFC3339), now.Format(time.RFC3339)).Scan(&prediction.Gate, &prediction.Samples, &prediction.Share)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (prediction.Samples < 3 || prediction.Share < 0.60) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &prediction, nil
}
