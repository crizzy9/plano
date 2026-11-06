package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const (
	advisorySupersessionRetention = 7 * time.Hour
	advisorySupersessionLead      = 30 * time.Second
)

const advisorySupersessionSchema = `
CREATE TABLE IF NOT EXISTS advisory_supersessions (
  source TEXT NOT NULL, airport TEXT NOT NULL, predecessor_id TEXT NOT NULL, observed_ns INTEGER NOT NULL, successor_id TEXT NOT NULL,
  PRIMARY KEY(source,airport,predecessor_id,observed_ns,successor_id)
);
CREATE INDEX IF NOT EXISTS advisory_supersessions_observed ON advisory_supersessions(observed_ns);
CREATE INDEX IF NOT EXISTS advisory_supersessions_successor ON advisory_supersessions(source,successor_id);`

// Once an observation establishes a link, a later poll or callback cannot
// retarget the same successor ID within the retained history.
func storeAdvisorySupersessions(ctx context.Context, tx *sql.Tx, incomingSQL string, args ...any) error {
	var conflict bool
	if err := tx.QueryRowContext(ctx, `WITH incoming(source,airport,predecessor_id,observed_ns,successor_id) AS (`+incomingSQL+`)
SELECT EXISTS(SELECT 1 FROM incoming i JOIN advisory_supersessions r USING(source,successor_id)
WHERE i.airport<>r.airport OR i.predecessor_id<>r.predecessor_id)`, args...).Scan(&conflict); err != nil {
		return err
	}
	if conflict {
		return fmt.Errorf("%w: advisory replacement conflicts with a known link", feeds.ErrInvalidQuery)
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO advisory_supersessions(source,airport,predecessor_id,observed_ns,successor_id) `+incomingSQL, args...)
	return err
}

func (s *Store) filterSupersededAdvisories(ctx context.Context, advisories []feeds.Advisory, now time.Time) ([]feeds.Advisory, error) {
	return filterSupersededAdvisories(ctx, s.db, advisories, now)
}

func filterSupersededAdvisories(ctx context.Context, db advisoryQueryer, advisories []feeds.Advisory, now time.Time) ([]feeds.Advisory, error) {
	if len(advisories) == 0 {
		return advisories, nil
	}
	var active bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM advisory_supersessions WHERE observed_ns<=?)`,
		now.UnixNano()).Scan(&active); err != nil || !active {
		return advisories, err
	}
	type candidate struct {
		Source   string `json:"source"`
		Airport  string `json:"airport"`
		ID       string `json:"id"`
		Observed int64  `json:"observed"`
	}
	candidates := make([]candidate, len(advisories))
	for i, a := range advisories {
		candidates[i] = candidate{a.Source, a.Airport, a.ID, a.ObservedAt.UnixNano()}
	}
	payload, err := json.Marshal(candidates)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT c.key FROM json_each(?) c WHERE EXISTS (
SELECT 1 FROM advisory_supersessions r WHERE r.source=json_extract(c.value,'$.source')
AND r.airport=json_extract(c.value,'$.airport') AND r.predecessor_id=json_extract(c.value,'$.id')
AND r.observed_ns BETWEEN json_extract(c.value,'$.observed') AND ?)`, string(payload), now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var removed []bool
	for rows.Next() {
		var index int
		if err := rows.Scan(&index); err != nil {
			return nil, err
		}
		if removed == nil {
			removed = make([]bool, len(advisories))
		}
		removed[index] = true
	}
	if err := rows.Err(); err != nil || removed == nil {
		return advisories, err
	}
	kept := advisories[:0]
	for i, a := range advisories {
		if !removed[i] {
			kept = append(kept, a)
		}
	}
	clear(advisories[len(kept):])
	return kept, nil
}
