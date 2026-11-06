package core

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const tfmsProgramCancellationSchema = `
CREATE TABLE IF NOT EXISTS tfms_program_cancellations (
  id TEXT PRIMARY KEY NOT NULL,
  program_id TEXT NOT NULL,
  airport TEXT NOT NULL,
  observed_ns INTEGER NOT NULL,
  starts_ns INTEGER NOT NULL,
  ends_ns INTEGER NOT NULL CHECK(ends_ns>starts_ns)
);
CREATE INDEX IF NOT EXISTS tfms_program_cancellations_airport ON tfms_program_cancellations(airport,observed_ns);
CREATE INDEX IF NOT EXISTS tfms_program_cancellations_ends ON tfms_program_cancellations(ends_ns);`

type tfmsProgramCancellation struct {
	programID                    string
	observedNS, startsNS, endsNS int64
}

func recordTFMSProgramCancellation(ctx context.Context, tx *sql.Tx, a feeds.Advisory) error {
	if a.StartsAt == nil || a.EndsAt == nil || !a.EndsAt.After(*a.StartsAt) {
		return fmt.Errorf("%w: TFMS cancellation needs a valid program window", feeds.ErrInvalidQuery)
	}
	observed, starts, ends := a.ObservedAt.UnixNano(), a.StartsAt.UnixNano(), a.EndsAt.UnixNano()
	result, err := tx.ExecContext(ctx, `INSERT INTO tfms_program_cancellations(id,program_id,airport,observed_ns,starts_ns,ends_ns)
VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, a.ID, a.TFMSProgramID, a.Airport, observed, starts, ends)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 0 {
		return err
	}
	var same bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tfms_program_cancellations
WHERE id=? AND program_id=? AND airport=? AND observed_ns=? AND starts_ns=? AND ends_ns=?)`,
		a.ID, a.TFMSProgramID, a.Airport, observed, starts, ends).Scan(&same); err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("%w: TFMS cancellation conflicts with a known advisory", feeds.ErrInvalidQuery)
	}
	return nil
}

func loadTFMSProgramCancellations(ctx context.Context, db flightAdvisoryRowsQueryer, airport, icao string, now time.Time) ([]tfmsProgramCancellation, error) {
	if airport == "" && icao == "" {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT program_id,observed_ns,starts_ns,ends_ns FROM tfms_program_cancellations
WHERE airport IN (?,?) AND observed_ns<=? AND ends_ns>?`, airport, icao, now.UnixNano(), now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cancellations []tfmsProgramCancellation
	for rows.Next() {
		var c tfmsProgramCancellation
		if err := rows.Scan(&c.programID, &c.observedNS, &c.startsNS, &c.endsNS); err != nil {
			return nil, err
		}
		cancellations = append(cancellations, c)
	}
	return cancellations, rows.Err()
}

func latestTFMSProgramCancellation(cancellations []tfmsProgramCancellation, programID string, operation time.Time) time.Time {
	var latest time.Time
	target := operation.UnixNano()
	for _, c := range cancellations {
		if c.programID == programID && c.startsNS <= target && target < c.endsNS {
			observed := time.Unix(0, c.observedNS)
			if observed.After(latest) {
				latest = observed
			}
		}
	}
	return latest
}
