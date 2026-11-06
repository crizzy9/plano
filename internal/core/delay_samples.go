package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"
	"path/filepath"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
)

func recordDelaySample(ctx context.Context, tx *sql.Tx, flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, decision time.Time) error {
	if flight.Status == "cancelled" {
		return nil
	}
	if departed := flight.ActualDeparture; departed != nil && !departed.After(flight.ObservedAt) {
		_, err := tx.ExecContext(ctx, `UPDATE delay_samples SET departed_ns=?,label_observed_ns=? WHERE flight_id=? AND observed_ns<?
AND scheduled_ns>=? AND (departed_ns IS NULL OR departed_ns<>?)`, departed.UnixNano(), flight.ObservedAt.UnixNano(), flight.ID, departed.UnixNano(), departed.Add(-24*time.Hour).UnixNano(), departed.UnixNano())
		return err
	}
	if flight.ScheduledDeparture == nil {
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
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT flight_id,observed_ns,scheduled_ns,features,departed_ns,label_observed_ns
FROM (SELECT * FROM delay_samples WHERE departed_ns IS NOT NULL ORDER BY observed_ns DESC,flight_id LIMIT 100000) ORDER BY observed_ns,flight_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	encoder := json.NewEncoder(dst)
	for rows.Next() {
		var sample predict.Example
		var observed, scheduled, departed, labelObserved int64
		var raw []byte
		if err := rows.Scan(&sample.FlightID, &observed, &scheduled, &raw, &departed, &labelObserved); err != nil {
			return err
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
