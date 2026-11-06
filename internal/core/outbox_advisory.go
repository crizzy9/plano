package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

const outboxAdvisorySchema = `
CREATE TABLE IF NOT EXISTS outbox_advisory_evidence (
  outbox_id INTEGER NOT NULL REFERENCES outbox(id) ON DELETE CASCADE,
  source TEXT NOT NULL, advisory_id TEXT NOT NULL, observed_ns INTEGER NOT NULL, state_hash TEXT NOT NULL,
  PRIMARY KEY(outbox_id,source,advisory_id)
);
CREATE TABLE IF NOT EXISTS pushed_advisory_predecessors (
  source TEXT NOT NULL, id TEXT NOT NULL, observed_ns INTEGER NOT NULL, expires_ns INTEGER NOT NULL, data TEXT NOT NULL,
  PRIMARY KEY(source,id,observed_ns), FOREIGN KEY(source,id) REFERENCES pushed_advisories(source,id) ON DELETE CASCADE
);`

func outboxAdvisoryRevised(ctx context.Context, db rowQueryer, jobID int64, now time.Time) (bool, error) {
	return advisoryEvidenceRevised(ctx, db, `SELECT source,advisory_id,observed_ns,state_hash FROM outbox_advisory_evidence WHERE outbox_id=?`, jobID, now)
}

func linkOutboxAdvisories(ctx context.Context, tx *sql.Tx, jobs *sql.Rows, evidence []forecastAdvisory) error {
	defer jobs.Close()
	var ids []int64
	for jobs.Next() {
		var id int64
		if err := jobs.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := jobs.Err(); err != nil {
		return err
	}
	if err := jobs.Close(); err != nil || len(ids) == 0 {
		return err
	}
	jobIDs, err := json.Marshal(ids)
	if err != nil {
		return err
	}
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
	return err
}
