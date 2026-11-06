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

const (
	maxDelaySampleLateness = 24 * time.Hour
	earlyDelayBoundary     = 2 * time.Hour
	earlyDelayMaxLead      = 25 * time.Hour
)

const delayEarlySamplesSchema = `
CREATE TABLE IF NOT EXISTS delay_early_samples (
  flight_id TEXT PRIMARY KEY REFERENCES flights(id), observed_ns INTEGER NOT NULL,
  scheduled_ns INTEGER NOT NULL, features TEXT NOT NULL, departed_ns INTEGER, label_observed_ns INTEGER
);
CREATE TABLE IF NOT EXISTS delay_early_provider_samples (
  provider_id TEXT PRIMARY KEY, observed_ns INTEGER NOT NULL, scheduled_ns INTEGER NOT NULL,
  features TEXT NOT NULL, departed_ns INTEGER, label_observed_ns INTEGER,
  outcome_observed_ns INTEGER NOT NULL DEFAULT 0
);`

// Table names are fixed here and are never taken from an export argument.
type delaySampleTables struct {
	local, provider string
}

var (
	standardDelaySampleTables = delaySampleTables{local: "delay_samples", provider: "delay_provider_samples"}
	earlyDelaySampleTables    = delaySampleTables{local: "delay_early_samples", provider: "delay_early_provider_samples"}
	delaySampleCohorts        = [...]delaySampleTables{standardDelaySampleTables, earlyDelaySampleTables}
)

func recordDelaySample(ctx context.Context, tx *sql.Tx, flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, decision time.Time, capture bool) error {
	if provider := flight.AeroProviderID(); provider != "" {
		return recordProviderDelaySample(ctx, tx, provider, flight, rotation, advisories, decision, capture)
	}
	// An unresolved schedule ID is a reference to a timetable entry. Its
	// eventual provider mapping cannot prove which flight a forecast described.
	if flight.Source == "flightaware_schedule" || flight.Source == "flightaware_aeroapi" {
		return nil
	}
	if flight.Status == "cancelled" || flight.ActualDeparture != nil {
		for _, tables := range delaySampleCohorts {
			if err := recordLocalDelayOutcome(ctx, tx, tables.local, flight); err != nil {
				return err
			}
		}
		return nil
	}
	if !capture {
		return nil
	}
	tables, raw, err := delaySampleFeatures(flight, rotation, advisories, decision)
	if err != nil || raw == nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO `+tables.local+`(flight_id,observed_ns,scheduled_ns,features) VALUES(?,?,?,?)`,
		flight.ID, decision.UnixNano(), flight.ScheduledDeparture.UnixNano(), string(raw))
	return err
}

func recordLocalDelayOutcome(ctx context.Context, tx *sql.Tx, table string, flight feeds.Flight) error {
	departed := flight.ActualDeparture
	if flight.Status == "cancelled" || departed.IsZero() || departed.After(flight.ObservedAt) {
		_, err := tx.ExecContext(ctx, `UPDATE `+table+` SET departed_ns=NULL,label_observed_ns=NULL
WHERE flight_id=? AND (departed_ns IS NOT NULL OR label_observed_ns IS NOT NULL)`, flight.ID)
		return err
	}
	// Check the corrected outcome against the forecast's original schedule.
	// Repeated observations of a valid gate-out retain its first label time.
	const eligible = `observed_ns < @departed AND scheduled_ns >= @earliest_schedule`
	_, err := tx.ExecContext(ctx, `UPDATE `+table+`
SET departed_ns=CASE WHEN `+eligible+` THEN @departed END,
    label_observed_ns=CASE WHEN `+eligible+` THEN @label_observed END
WHERE flight_id=@id AND (departed_ns IS NOT @departed OR label_observed_ns IS NULL
 OR label_observed_ns < @departed OR label_observed_ns > @label_observed OR NOT (`+eligible+`))`,
		sql.Named("departed", departed.UnixNano()), sql.Named("earliest_schedule", departed.Add(-maxDelaySampleLateness).UnixNano()),
		sql.Named("label_observed", flight.ObservedAt.UnixNano()), sql.Named("id", flight.ID))
	return err
}

func delaySampleFeatures(flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory, decision time.Time) (delaySampleTables, []byte, error) {
	if flight.ScheduledDeparture == nil {
		return delaySampleTables{}, nil, nil
	}
	lead := flight.ScheduledDeparture.Sub(decision)
	if lead < 30*time.Minute || lead > earlyDelayMaxLead {
		return delaySampleTables{}, nil, nil
	}
	tables := standardDelaySampleTables
	if lead > earlyDelayBoundary {
		tables = earlyDelaySampleTables
	}
	features, ok := ExtractDelayFeatures(flight, rotation, advisories, decision)
	if !ok || max(features.AirlineDelayMinutes, features.InboundDelayMinutes, features.AirportDelayMinutes, features.GroundStopMinutes) > 1440 {
		return delaySampleTables{}, nil, nil
	}
	raw, err := json.Marshal(features)
	return tables, raw, err
}

// ExportDelaySamples opens the operator database read-only and streams training
// inputs without passenger records, email, flight numbers, or booking codes.
func ExportDelaySamples(ctx context.Context, path string, dst io.Writer) error {
	return exportDelaySamples(ctx, path, dst, standardDelaySampleTables)
}

// ExportEarlyDelaySamples streams the separate cohort observed more than two
// hours before scheduled departure.
func ExportEarlyDelaySamples(ctx context.Context, path string, dst io.Writer) error {
	return exportDelaySamples(ctx, path, dst, earlyDelaySampleTables)
}

func exportDelaySamples(ctx context.Context, path string, dst io.Writer, tables delaySampleTables) error {
	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var hasLocalSamples, hasProviderSamples bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?),
EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)`, tables.local, tables.provider).Scan(&hasLocalSamples, &hasProviderSamples); err != nil {
		return err
	}
	if tables == earlyDelaySampleTables && !hasLocalSamples && !hasProviderSamples {
		return nil
	}
	// A pre-upgrade backup can still export verifiable direct records. The
	// mutable alias index cannot establish a legacy alias's capture identity.
	candidates := `SELECT l.flight_id AS identity,l.observed_ns,l.scheduled_ns,l.features,l.departed_ns,l.label_observed_ns,0 AS outcome_observed_ns,
CASE WHEN k.provider_id=l.flight_id OR json_extract(saved.snapshot,'$.source')='flightaware_aeroapi' THEN 1 ELSE 0 END AS provider
FROM ` + tables.local + ` l JOIN flights saved ON saved.id=l.flight_id
LEFT JOIN aero_instance_keys k ON k.flight_id=l.flight_id
WHERE l.flight_id NOT GLOB 'scheduled_*' AND (k.provider_id IS NULL OR k.provider_id=l.flight_id)
AND (COALESCE(json_extract(saved.snapshot,'$.source'),'')<>'flightaware_schedule' OR k.provider_id=l.flight_id)`
	if hasProviderSamples {
		// A withdrawn canonical label must also hide the old direct record.
		candidates += ` AND NOT EXISTS(SELECT 1 FROM ` + tables.provider + ` p WHERE p.provider_id=l.flight_id)
UNION ALL SELECT provider_id,observed_ns,scheduled_ns,features,departed_ns,label_observed_ns,outcome_observed_ns,1 FROM ` + tables.provider
	}
	rows, err := db.QueryContext(ctx, `WITH candidates AS (`+candidates+`), recent AS (
SELECT * FROM candidates WHERE departed_ns>observed_ns AND label_observed_ns>=departed_ns
AND scheduled_ns>=departed_ns-? ORDER BY observed_ns DESC,identity LIMIT 100000)
SELECT s.identity,s.observed_ns,s.scheduled_ns,s.features,s.departed_ns,s.label_observed_ns,
       MAX(s.label_observed_ns,s.outcome_observed_ns),
       COALESCE(f.observed_ns,0),COALESCE(json_extract(f.snapshot,'$.status'),''),json_extract(f.snapshot,'$.actualDeparture')
FROM recent s LEFT JOIN flights f ON f.id=CASE WHEN s.provider=0 THEN s.identity ELSE (
 SELECT latest.id FROM flights latest WHERE latest.id IN (
  SELECT k.flight_id FROM aero_instance_keys k WHERE k.provider_id=s.identity
  UNION ALL SELECT s.identity WHERE NOT EXISTS(SELECT 1 FROM aero_instance_keys k WHERE k.flight_id=s.identity)
 ) AND json_extract(latest.snapshot,'$.source')='flightaware_aeroapi'
 AND (json_extract(latest.snapshot,'$.status')='cancelled' OR json_extract(latest.snapshot,'$.actualDeparture') IS NOT NULL)
 ORDER BY latest.observed_ns DESC,json_extract(latest.snapshot,'$.status')='cancelled' DESC,latest.id LIMIT 1
) END ORDER BY s.observed_ns,s.identity`, int64(maxDelaySampleLateness))
	if err != nil {
		return err
	}
	defer rows.Close()
	encoder := json.NewEncoder(dst)
	for rows.Next() {
		var sample predict.Example
		var observed, scheduled, departed, labelObserved, outcomeObserved, currentObserved int64
		var currentStatus string
		var currentDeparture sql.NullString
		var raw []byte
		if err := rows.Scan(&sample.FlightID, &observed, &scheduled, &raw, &departed, &labelObserved,
			&outcomeObserved, &currentObserved, &currentStatus, &currentDeparture); err != nil {
			return err
		}
		// Older collectors could leave the original label after the provider
		// corrected it. A newer snapshot without gate-out is still inconclusive.
		if currentObserved >= outcomeObserved {
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
