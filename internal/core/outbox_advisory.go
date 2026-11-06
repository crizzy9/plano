package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const outboxAdvisorySchema = `
CREATE TABLE IF NOT EXISTS outbox_advisory_evidence (
  outbox_id INTEGER NOT NULL REFERENCES outbox(id) ON DELETE CASCADE,
  source TEXT NOT NULL, advisory_id TEXT NOT NULL, observed_ns INTEGER NOT NULL, state_hash TEXT NOT NULL,
  PRIMARY KEY(outbox_id,source,advisory_id)
);
CREATE TABLE IF NOT EXISTS outbox_advisory_scopes (
  outbox_id INTEGER NOT NULL, source TEXT NOT NULL, advisory_id TEXT NOT NULL, airport TEXT NOT NULL,
  PRIMARY KEY(outbox_id,source,advisory_id),
  FOREIGN KEY(outbox_id,source,advisory_id) REFERENCES outbox_advisory_evidence(outbox_id,source,advisory_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS pushed_advisory_predecessors (
  source TEXT NOT NULL, id TEXT NOT NULL, observed_ns INTEGER NOT NULL, expires_ns INTEGER NOT NULL, data TEXT NOT NULL,
  PRIMARY KEY(source,id,observed_ns), FOREIGN KEY(source,id) REFERENCES pushed_advisories(source,id) ON DELETE CASCADE
);`

func outboxAdvisoryRevised(ctx context.Context, db rowQueryer, jobID int64, now time.Time, requireCurrent bool) (bool, error) {
	return advisoryEvidenceRevised(ctx, db, `SELECT source,advisory_id,observed_ns,state_hash,COALESCE(airport,'')
FROM outbox_advisory_evidence LEFT JOIN outbox_advisory_scopes USING(outbox_id,source,advisory_id) WHERE outbox_id=?`, jobID, now, requireCurrent)
}

func tfmsOutboxOperationCancelled(ctx context.Context, db rowQueryer, jobID int64, target, now time.Time) (bool, error) {
	var cancelled bool
	operation := target.UnixNano()
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outbox_advisory_evidence e
JOIN tfms_program_cancellations c ON c.program_id=e.advisory_id
WHERE e.outbox_id=? AND e.source=? AND c.observed_ns>e.observed_ns AND c.observed_ns<=?
  AND c.starts_ns<=? AND c.ends_ns>?)`, jobID, feeds.FAASWIM, now.UnixNano(), operation, operation).Scan(&cancelled)
	return cancelled, err
}

func readOutboxJobIDs(jobs *sql.Rows) ([]byte, error) {
	defer jobs.Close()
	var ids []int64
	for jobs.Next() {
		var id int64
		if err := jobs.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := jobs.Err(); err != nil {
		return nil, err
	}
	if err := jobs.Close(); err != nil || len(ids) == 0 {
		return nil, err
	}
	return json.Marshal(ids)
}

func linkOutboxAdvisories(ctx context.Context, tx *sql.Tx, jobIDs []byte, evidence []forecastAdvisory) error {
	observations, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox_advisory_evidence(outbox_id,source,advisory_id,observed_ns,state_hash)
SELECT jobs.value,json_extract(refs.value,'$.source'),json_extract(refs.value,'$.id'),
 json_extract(refs.value,'$.observedNs'),COALESCE(json_extract(refs.value,'$.stateHash'),'')
FROM json_each(?) jobs CROSS JOIN json_each(?) refs
WHERE json_extract(refs.value,'$.source')<>'' AND json_extract(refs.value,'$.id')<>''
ORDER BY json_extract(refs.value,'$.observedNs')`, string(jobIDs), string(observations))
	if err != nil {
		return err
	}
	// Match the evidence that won deduplication above, including its state and
	// observation. A duplicate row must not lend its scope to different evidence.
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox_advisory_scopes(outbox_id,source,advisory_id,airport)
SELECT e.outbox_id,e.source,e.advisory_id,json_extract(refs.value,'$.airport')
FROM json_each(?) jobs CROSS JOIN json_each(?) refs JOIN outbox_advisory_evidence e
 ON e.outbox_id=jobs.value AND e.source=json_extract(refs.value,'$.source') AND e.advisory_id=json_extract(refs.value,'$.id')
 AND e.observed_ns=json_extract(refs.value,'$.observedNs') AND e.state_hash=COALESCE(json_extract(refs.value,'$.stateHash'),'')
WHERE json_extract(refs.value,'$.airport')<>''`, string(jobIDs), string(observations))
	return err
}
