package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const advisoryWithdrawalSchema = `
CREATE TABLE IF NOT EXISTS advisory_withdrawals (
  source TEXT NOT NULL, id TEXT NOT NULL, observed_ns INTEGER NOT NULL, signed INTEGER NOT NULL,
  cleared_ns INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(source,id,observed_ns)
);
CREATE INDEX IF NOT EXISTS outbox_advisory_evidence_by_source ON outbox_advisory_evidence(source,advisory_id,observed_ns);`

// An effective withdrawal invalidates its own source timestamp until an
// authoritative correction clears it. Earlier revisions stay withdrawn.
const effectiveAdvisoryWithdrawal = `(w.cleared_ns=0 OR w.cleared_ns>=w.observed_ns)`
const advisoryWithdrawalCutoff = `(w.observed_ns - CASE WHEN w.cleared_ns=0 THEN 0 ELSE 1 END)`

func wholeAdvisoryWithdrawal(a feeds.Advisory) bool {
	return a.SupersedesID == "" && a.Kind != feeds.AdvisoryTFMSCancellation && advisoryCancelled(a.Kind)
}

func recordAdvisoryWithdrawal(ctx context.Context, tx *sql.Tx, a feeds.Advisory, now time.Time, signed bool) error {
	if a.Source == "" || a.ID == "" || !a.ObservationAllowed(now) {
		return nil
	}
	evidence, err := json.Marshal([]forecastAdvisory{advisoryEvidence(a)})
	if err != nil {
		return err
	}
	// Apply the existing source order, including the signed winner at a tie.
	// A future revision can establish history before its effective time.
	evaluateAt := now
	if a.ObservedAt.After(now) {
		evaluateAt = a.ObservedAt
	}
	revised, err := advisoryEvidenceRevised(ctx, tx, jsonAdvisoryEvidenceSQL, string(evidence), evaluateAt, false, false)
	if err != nil || revised {
		return err
	}
	observed := a.ObservedAt.UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO advisory_withdrawals(source,id,observed_ns,signed) VALUES(?,?,?,?)
ON CONFLICT(source,id,observed_ns) DO UPDATE SET signed=MAX(signed,excluded.signed),cleared_ns=0`, a.Source, a.ID, observed, signed); err != nil {
		return err
	}
	if a.ObservedAt.After(now) {
		return nil
	}
	return retireWithdrawnAdvisoryAlerts(ctx, tx, a)
}

func applyObservedAdvisoryWithdrawal(ctx context.Context, tx *sql.Tx, a feeds.Advisory, now time.Time) error {
	observed := a.ObservedAt.UnixNano()
	// Flight observations can carry an accepted future withdrawal once it
	// becomes effective. Resolve its provenance from the source caches.
	var signed, polled bool
	t := max(now.UnixNano(), observed)
	err := tx.QueryRowContext(ctx, `WITH signed AS (`+currentPushedAdvisories+`)
SELECT EXISTS(SELECT 1 FROM signed WHERE source=?4 AND id=?5 AND observed_ns=?6 AND json_extract(data,'$.kind')=?7),
 EXISTS(SELECT 1 FROM polled_flight_notices WHERE source=?4 AND id=?5 AND observed_ns=?6 AND json_extract(data,'$.kind')=?7)`,
		t, t, t, a.Source, a.ID, observed, a.Kind).Scan(&signed, &polled)
	if err != nil || !signed && !polled {
		return err
	}
	return recordAdvisoryWithdrawal(ctx, tx, a, now, signed)
}

func retireWithdrawnAdvisoryAlerts(ctx context.Context, tx *sql.Tx, a feeds.Advisory) error {
	// Direct warnings collapse by advisory family. Other alert jobs can also
	// cite this advisory, but keep their own timeline event and validity rules.
	_, err := tx.ExecContext(ctx, `DELETE FROM outbox AS o WHERE o.done=0 AND o.id IN (
 SELECT outbox_id FROM outbox_advisory_evidence WHERE source=? AND advisory_id=? AND observed_ns<=?)
AND json_extract(o.message,'$.Kind')='alert'
AND NOT EXISTS (SELECT 1 FROM events e WHERE e.id=json_extract(o.message,'$.CollapseID') AND e.flight_id=o.flight_id)`,
		a.Source, a.ID, a.ObservedAt.UnixNano())
	return err
}

func advisoryReopened(ctx context.Context, tx *sql.Tx, evidence forecastAdvisory, priorObserved, priorApplied, applied int64) (bool, error) {
	var reopened bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM advisory_withdrawals w
WHERE w.source=?1 AND w.id=?2 AND `+effectiveAdvisoryWithdrawal+`
AND ((w.observed_ns>=?3 AND w.observed_ns<?4) OR (w.observed_ns=?4 AND w.cleared_ns>?5 AND w.cleared_ns<=?6)))`,
		evidence.Source, evidence.ID, priorObserved, evidence.ObservedNS, priorApplied, applied).Scan(&reopened)
	return reopened, err
}

func withdrawnAdvisoryAlert(ctx context.Context, db rowQueryer, jobID int64, now time.Time) (bool, error) {
	var withdrawn bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outbox_advisory_evidence e
JOIN advisory_withdrawals w ON w.source=e.source AND w.id=e.advisory_id
WHERE e.outbox_id=? AND `+effectiveAdvisoryWithdrawal+` AND `+advisoryWithdrawalCutoff+`>=e.observed_ns AND w.observed_ns<=?)`,
		jobID, now.UnixNano()).Scan(&withdrawn)
	return withdrawn, err
}
