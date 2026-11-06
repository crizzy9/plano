package core

import (
	"context"
	"database/sql"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const delayProviderSamplesSchema = `
CREATE TABLE IF NOT EXISTS delay_provider_samples (
  provider_id TEXT PRIMARY KEY, observed_ns INTEGER NOT NULL, scheduled_ns INTEGER NOT NULL,
  features TEXT NOT NULL, departed_ns INTEGER, label_observed_ns INTEGER,
  outcome_observed_ns INTEGER NOT NULL DEFAULT 0
);`

// Older direct provider rows may predate the alias index. A contrary mapping
// takes precedence over that fallback, including when the local ID looks direct.
const providerDelayAliasesSQL = `WITH aliases(id) AS (
 SELECT flight_id FROM aero_instance_keys WHERE provider_id=@provider
 UNION ALL SELECT @provider WHERE NOT EXISTS (
  SELECT 1 FROM aero_instance_keys WHERE flight_id=@provider)
) `

func recordProviderDelaySample(ctx context.Context, tx *sql.Tx, provider string, flight feeds.Flight, rotation []feeds.Flight, advisories []feeds.Advisory,
	forecasts []AirportWeatherForecast, decision time.Time, capture bool) error {
	// A direct legacy ID proves which flight was sampled. A schedule alias's
	// current mapping does not prove what the alias meant at capture time.
	var adopted bool
	for _, tables := range delaySampleCohorts {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO `+tables.provider+`
(provider_id,observed_ns,scheduled_ns,features,departed_ns,label_observed_ns,outcome_observed_ns)
SELECT s.flight_id,s.observed_ns,s.scheduled_ns,s.features,s.departed_ns,s.label_observed_ns,COALESCE(s.label_observed_ns,0)
FROM `+tables.local+` s JOIN flights f ON f.id=s.flight_id
LEFT JOIN aero_instance_keys k ON k.flight_id=s.flight_id
WHERE s.flight_id=? AND substr(f.id,1,10)<>'scheduled_' AND (
 (json_extract(f.snapshot,'$.source')='flightaware_aeroapi' AND COALESCE(k.provider_id,f.id)=f.id)
 OR (json_extract(f.snapshot,'$.source')='flightaware_schedule' AND k.provider_id=f.id)
)`, provider)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		adopted = adopted || count != 0
	}
	var outcome providerDelayOutcome
	if flight.Status == "cancelled" || flight.ActualDeparture != nil {
		outcome.observe(flight.ObservedAt, flight.ActualDeparture, flight.Status == "cancelled")
	}
	if adopted || !outcome.observedAt.IsZero() {
		// Reconcile saved explicit evidence even if it predates this schema or
		// was stored while sample collection was off. A local alias can accept
		// an observation older than one already accepted for another alias.
		if err := outcome.readAliases(ctx, tx, provider); err != nil {
			return err
		}
		if !outcome.observedAt.IsZero() {
			for _, tables := range delaySampleCohorts {
				if err := outcome.apply(ctx, tx, provider, tables.provider); err != nil {
					return err
				}
			}
		}
	}
	if !capture || !outcome.observedAt.IsZero() {
		return nil
	}
	tables, raw, err := delaySampleFeatures(provider, flight, rotation, advisories, forecasts, decision)
	if err != nil || raw == nil {
		return err
	}
	_, err = tx.ExecContext(ctx, providerDelayAliasesSQL+`INSERT OR IGNORE INTO `+tables.provider+`
(provider_id,observed_ns,scheduled_ns,features)
SELECT @provider,@decision,@scheduled,@features
WHERE NOT EXISTS (SELECT 1 FROM aliases a JOIN flights f ON f.id=a.id
 WHERE f.observed_ns>=@observed AND json_extract(f.snapshot,'$.source')='flightaware_aeroapi'
 AND (json_extract(f.snapshot,'$.actualDeparture') IS NOT NULL OR json_extract(f.snapshot,'$.actualTakeoff') IS NOT NULL
  OR json_extract(f.snapshot,'$.status') IN ('cancelled','diverted','taxi_out','airborne','landed','arrived')))`,
		sql.Named("provider", provider), sql.Named("observed", flight.ObservedAt.UnixNano()), sql.Named("decision", decision.UnixNano()),
		sql.Named("scheduled", flight.ScheduledDeparture.UnixNano()), sql.Named("features", string(raw)))
	return err
}

// A nonzero observation with a nil departure withdraws a label. It still
// advances the watermark so older aliases cannot resurrect the old outcome.
type providerDelayOutcome struct {
	observedAt time.Time
	departure  *time.Time
}

func (o *providerDelayOutcome) observe(observedAt time.Time, departure *time.Time, cancelled bool) {
	if cancelled || departure != nil && (departure.IsZero() || departure.After(observedAt)) {
		departure = nil
	}
	if observedAt.After(o.observedAt) {
		o.observedAt, o.departure = observedAt, departure
	} else if observedAt.Equal(o.observedAt) && o.departure != nil && (departure == nil || !o.departure.Equal(*departure)) {
		// At a timestamp tie, contradictory evidence makes the outcome unusable.
		o.departure = nil
	}
}

func (o *providerDelayOutcome) readAliases(ctx context.Context, tx *sql.Tx, provider string) error {
	var since int64
	if !o.observedAt.IsZero() {
		since = o.observedAt.UnixNano()
	}
	rows, err := tx.QueryContext(ctx, providerDelayAliasesSQL+`SELECT f.observed_ns,
COALESCE(json_extract(f.snapshot,'$.status'),'')='cancelled',json_extract(f.snapshot,'$.actualDeparture')
FROM aliases a JOIN flights f ON f.id=a.id
WHERE f.observed_ns>=@since AND json_extract(f.snapshot,'$.source')='flightaware_aeroapi'
AND (json_extract(f.snapshot,'$.status')='cancelled' OR json_extract(f.snapshot,'$.actualDeparture') IS NOT NULL)
ORDER BY f.observed_ns DESC`, sql.Named("provider", provider), sql.Named("since", since))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var observedNS int64
		var cancelled bool
		var raw sql.NullString
		if err := rows.Scan(&observedNS, &cancelled, &raw); err != nil {
			return err
		}
		observedAt := time.Unix(0, observedNS).UTC()
		if observedAt.Before(o.observedAt) {
			break
		}
		var departure *time.Time
		if raw.Valid {
			if at, err := time.Parse(time.RFC3339Nano, raw.String); err == nil {
				departure = &at
			}
		}
		o.observe(observedAt, departure, cancelled)
	}
	return rows.Err()
}

func (o providerDelayOutcome) apply(ctx context.Context, tx *sql.Tx, provider, table string) error {
	var departed, earliestSchedule sql.NullInt64
	if o.departure != nil {
		departed = sql.NullInt64{Int64: o.departure.UnixNano(), Valid: true}
		earliestSchedule = sql.NullInt64{Int64: o.departure.Add(-maxDelaySampleLateness).UnixNano(), Valid: true}
	}
	const eligible = `observed_ns < @departed AND scheduled_ns >= @earliest_schedule
AND (@observed > outcome_observed_ns OR departed_ns IS @departed)`
	_, err := tx.ExecContext(ctx, `UPDATE `+table+`
SET departed_ns=CASE WHEN `+eligible+` THEN @departed END,
    label_observed_ns=CASE WHEN `+eligible+` THEN CASE
      WHEN departed_ns IS @departed AND label_observed_ns BETWEEN @departed AND @observed THEN label_observed_ns
      ELSE @observed END END,
    outcome_observed_ns=@observed
WHERE provider_id=@provider AND outcome_observed_ns<=@observed`,
		sql.Named("departed", departed), sql.Named("earliest_schedule", earliestSchedule),
		sql.Named("observed", o.observedAt.UnixNano()), sql.Named("provider", provider))
	return err
}
