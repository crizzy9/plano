package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

const maxDelaySampleLateness = 24 * time.Hour

func recordDelaySample(ctx context.Context, tx *sql.Tx, flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, decision time.Time, capture bool) error {
	departed := flight.ActualDeparture
	if flight.Status == "cancelled" || departed != nil && (departed.IsZero() || departed.After(flight.ObservedAt)) {
		_, err := tx.ExecContext(ctx, `UPDATE delay_samples SET departed_ns=NULL,label_observed_ns=NULL
WHERE flight_id=? AND (departed_ns IS NOT NULL OR label_observed_ns IS NOT NULL)`, flight.ID)
		return err
	}
	if departed != nil {
		// Check the corrected outcome against the forecast's original schedule.
		// Repeated observations of a valid gate-out retain its first label time.
		const eligible = `observed_ns < @departed AND scheduled_ns >= @earliest_schedule`
		_, err := tx.ExecContext(ctx, `UPDATE delay_samples
SET departed_ns=CASE WHEN `+eligible+` THEN @departed END,
    label_observed_ns=CASE WHEN `+eligible+` THEN @label_observed END
WHERE flight_id=@id AND (departed_ns IS NOT @departed OR label_observed_ns IS NULL
 OR label_observed_ns < @departed OR label_observed_ns > @label_observed OR NOT (`+eligible+`))`,
			sql.Named("departed", departed.UnixNano()), sql.Named("earliest_schedule", departed.Add(-maxDelaySampleLateness).UnixNano()),
			sql.Named("label_observed", flight.ObservedAt.UnixNano()), sql.Named("id", flight.ID))
		return err
	}
	if !capture || flight.ScheduledDeparture == nil {
		return nil
	}
	lead := flight.ScheduledDeparture.Sub(decision)
	if lead < 30*time.Minute || lead > 2*time.Hour {
		return nil
	}
	features, ok := ExtractDelayFeatures(flight, rotation, advisories, decision)
	if !ok || max(features.AirlineDelayMinutes, features.InboundDelayMinutes, features.AirportDelayMinutes, features.GroundStopMinutes) > 1440 {
		return nil
	}
	raw, err := json.Marshal(features)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO delay_samples(flight_id,observed_ns,scheduled_ns,features) VALUES(?,?,?,?)`, flight.ID, decision.UnixNano(), flight.ScheduledDeparture.UnixNano(), string(raw))
	return err
}

// ExportDelaySamples opens the operator database read-only and streams training
// inputs without passenger records, email, flight numbers, or booking codes.
func ExportDelaySamples(ctx context.Context, path string, dst io.Writer) error {
	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT s.flight_id,s.observed_ns,s.scheduled_ns,s.features,s.departed_ns,s.label_observed_ns,
       f.observed_ns,COALESCE(json_extract(f.snapshot,'$.status'),''),json_extract(f.snapshot,'$.actualDeparture')
FROM (SELECT * FROM delay_samples WHERE departed_ns>observed_ns AND label_observed_ns>=departed_ns
 AND scheduled_ns>=departed_ns-? ORDER BY observed_ns DESC,flight_id LIMIT 100000) s
JOIN flights f ON f.id=s.flight_id ORDER BY s.observed_ns,s.flight_id`, int64(maxDelaySampleLateness))
	if err != nil {
		return err
	}
	defer rows.Close()
	encoder := json.NewEncoder(dst)
	for rows.Next() {
		var sample predict.Example
		var observed, scheduled, departed, labelObserved, currentObserved int64
		var currentStatus string
		var currentDeparture sql.NullString
		var raw []byte
		if err := rows.Scan(&sample.FlightID, &observed, &scheduled, &raw, &departed, &labelObserved,
			&currentObserved, &currentStatus, &currentDeparture); err != nil {
			return err
		}
		// Older collectors could leave the original label after the provider
		// corrected it. A newer snapshot without gate-out is still inconclusive.
		if currentObserved >= labelObserved {
			if currentStatus == "cancelled" {
				continue
			}
			if currentDeparture.Valid {
				actual, err := time.Parse(time.RFC3339Nano, currentDeparture.String)
				if err != nil || !actual.Equal(time.Unix(0, departed)) {
					continue
				}
			}
		}
		if err := json.Unmarshal(raw, &sample.Features); err != nil {
			return err
		}
		sample.FlightID = hash("delay-sample\x00" + sample.FlightID)
		sample.ObservedAt, sample.ScheduledDeparture, sample.DepartedAt = time.Unix(0, observed).UTC(), time.Unix(0, scheduled).UTC(), time.Unix(0, departed).UTC()
		label := time.Unix(0, labelObserved).UTC()
		sample.LabelObservedAt = &label
		if err := encoder.Encode(sample); err != nil {
			return err
		}
	}
	return rows.Err()
}
