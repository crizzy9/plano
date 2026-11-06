package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

const connectionMonitorSchema = `
CREATE TABLE IF NOT EXISTS connection_monitors (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  owner TEXT NOT NULL, inbound TEXT NOT NULL, outbound TEXT NOT NULL,
  risk TEXT NOT NULL CHECK(risk IN ('Relaxed','Tight','Risky','Missed')),
  due_ns INTEGER NOT NULL, monitor_until_ns INTEGER NOT NULL,
  retire_ns INTEGER NOT NULL DEFAULT 0, needs_check INTEGER NOT NULL DEFAULT 0,
  UNIQUE(owner,inbound,outbound), CHECK(inbound<>outbound),
  FOREIGN KEY(owner,inbound) REFERENCES tracked(owner,flight_id) ON DELETE CASCADE,
  FOREIGN KEY(owner,outbound) REFERENCES tracked(owner,flight_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS connection_monitors_due ON connection_monitors(due_ns,id);
CREATE INDEX IF NOT EXISTS connection_monitors_inbound ON connection_monitors(inbound,owner);
CREATE INDEX IF NOT EXISTS connection_monitors_outbound ON connection_monitors(outbound,owner);
CREATE INDEX IF NOT EXISTS flights_by_gate_history ON flights(
  json_extract(snapshot,'$.number'), json_extract(snapshot,'$.origin'), json_extract(snapshot,'$.destination')
);
CREATE TABLE IF NOT EXISTS connection_monitor_jobs (
  outbox_id INTEGER PRIMARY KEY REFERENCES outbox(id) ON DELETE CASCADE,
  monitor_id INTEGER NOT NULL REFERENCES connection_monitors(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS connection_monitor_jobs_monitor ON connection_monitor_jobs(monitor_id);
CREATE TRIGGER IF NOT EXISTS connection_monitors_refresh AFTER UPDATE OF snapshot ON flights
BEGIN
  UPDATE connection_monitors SET due_ns=0,needs_check=1 WHERE inbound=NEW.id AND due_ns<>0;
  UPDATE connection_monitors SET due_ns=0,needs_check=1 WHERE outbound=NEW.id AND due_ns<>0;
END;
CREATE TRIGGER IF NOT EXISTS connection_monitors_remove_alerts BEFORE DELETE ON connection_monitors
BEGIN
  DELETE FROM outbox WHERE done=0 AND id IN (
    SELECT outbox_id FROM connection_monitor_jobs WHERE monitor_id=OLD.id
  );
END;
`

// A linked alert waits until the latest snapshots have been evaluated.
const dirtyConnectionAlert = `EXISTS (
  SELECT 1 FROM connection_monitor_jobs j JOIN connection_monitors m ON m.id=j.monitor_id
  WHERE j.outbox_id=o.id AND m.needs_check=1
)`

const (
	connectionMonitorLimit = 32
	connectionCheckRetry   = 30 * time.Second
	connectionCheckMax     = 5 * time.Minute
)

var (
	ErrInvalidConnection = errors.New("invalid or unavailable connection monitor")
	connectionRisks      = [...]string{"Relaxed", "Tight", "Risky", "Missed"}
)

type ConnectionGatePredictor func(context.Context, feeds.Flight, bool, time.Time) (*GatePrediction, error)

type ConnectionAssessment struct {
	Risk         string
	Monitorable  bool
	Incomplete   bool
	AlertUntil   time.Time
	NextCheck    time.Time
	MonitorUntil time.Time
}

// ConnectionEvaluator uses only the passed snapshots and gate predictor. Both
// are read from the same transaction as the monitor update.
type ConnectionEvaluator func(context.Context, feeds.Flight, feeds.Flight, time.Time, ConnectionGatePredictor) (ConnectionAssessment, error)

type ConnectionMonitor struct {
	Inbound  string `json:"inbound"`
	Outbound string `json:"outbound"`
	Risk     string `json:"risk"`
}

func (s *Store) TrackedConnection(ctx context.Context, owner, inID, outID string) (feeds.Flight, feeds.Flight, error) {
	return trackedConnection(ctx, s.db, owner, inID, outID)
}

func trackedConnection(ctx context.Context, db rowQueryer, owner, inID, outID string) (feeds.Flight, feeds.Flight, error) {
	var in, out feeds.Flight
	var inRaw, outRaw []byte
	err := db.QueryRowContext(ctx, `SELECT inbound.snapshot,outbound.snapshot
FROM tracked ti JOIN flights inbound ON inbound.id=ti.flight_id
JOIN tracked tor ON tor.owner=ti.owner JOIN flights outbound ON outbound.id=tor.flight_id
WHERE ti.owner=? AND ti.flight_id=? AND tor.flight_id=?`, owner, inID, outID).Scan(&inRaw, &outRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return in, out, ErrNotFound
	}
	if err != nil {
		return in, out, err
	}
	if err := json.Unmarshal(inRaw, &in); err != nil {
		return in, out, err
	}
	if err := json.Unmarshal(outRaw, &out); err != nil {
		return in, out, err
	}
	return in, out, nil
}

func assessTrackedConnection(ctx context.Context, tx *sql.Tx, owner, inID, outID string, now time.Time, evaluate ConnectionEvaluator) (ConnectionAssessment, error) {
	in, out, err := trackedConnection(ctx, tx, owner, inID, outID)
	if err != nil {
		return ConnectionAssessment{}, err
	}
	return evaluate(ctx, in, out, now, func(ctx context.Context, flight feeds.Flight, arrival bool, now time.Time) (*GatePrediction, error) {
		return predictGate(ctx, tx, flight, arrival, now)
	})
}

func (s *Store) MonitorConnection(ctx context.Context, owner, inID, outID string, now time.Time, evaluate ConnectionEvaluator) error {
	if inID == "" || outID == "" || inID == outID {
		return ErrInvalidConnection
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	active, err := connectionMonitored(ctx, tx, owner, inID, outID)
	if err != nil || active {
		return err
	}
	assessment, err := assessTrackedConnection(ctx, tx, owner, inID, outID, now, evaluate)
	if err != nil {
		return err
	}
	if assessment.Incomplete || !assessment.Monitorable || !assessment.MonitorUntil.After(now) || !slices.Contains(connectionRisks[:], assessment.Risk) {
		return ErrInvalidConnection
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM connection_monitors WHERE owner=? AND retire_ns=0`, owner).Scan(&count); err != nil {
		return err
	}
	if count >= connectionMonitorLimit {
		return ErrInvalidConnection
	}
	// A terminal alert can outlive a monitor. Explicitly re-enabling that
	// pair after a correction starts a fresh baseline and withdraws that alert.
	if _, err := tx.ExecContext(ctx, `DELETE FROM connection_monitors WHERE owner=? AND inbound=? AND outbound=?`, owner, inID, outID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connection_monitors(owner,inbound,outbound,risk,due_ns,monitor_until_ns) VALUES(?,?,?,?,?,?)`,
		owner, inID, outID, assessment.Risk, connectionCheckTime(assessment, now).UnixNano(), assessment.MonitorUntil.UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UnmonitorConnection(ctx context.Context, owner, inID, outID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connection_monitors WHERE owner=? AND inbound=? AND outbound=?`, owner, inID, outID)
	return err
}

func (s *Store) ConnectionMonitors(ctx context.Context, owner string) ([]ConnectionMonitor, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT inbound,outbound,risk FROM connection_monitors WHERE owner=? AND retire_ns=0 ORDER BY id DESC LIMIT ?`, owner, connectionMonitorLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ConnectionMonitor, 0)
	for rows.Next() {
		var monitor ConnectionMonitor
		if err := rows.Scan(&monitor.Inbound, &monitor.Outbound, &monitor.Risk); err != nil {
			return nil, err
		}
		result = append(result, monitor)
	}
	return result, rows.Err()
}

func (s *Store) ConnectionMonitored(ctx context.Context, owner, inID, outID string) (bool, error) {
	return connectionMonitored(ctx, s.db, owner, inID, outID)
}

func connectionMonitored(ctx context.Context, db rowQueryer, owner, inID, outID string) (bool, error) {
	var active bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connection_monitors WHERE owner=? AND inbound=? AND outbound=? AND retire_ns=0)`, owner, inID, outID).Scan(&active)
	return active, err
}

func connectionCheckTime(assessment ConnectionAssessment, now time.Time) time.Time {
	if !assessment.NextCheck.After(now) {
		assessment.NextCheck = now.Add(connectionCheckRetry)
	}
	next := min(assessment.NextCheck.UnixNano(), now.Add(connectionCheckMax).UnixNano())
	if !assessment.MonitorUntil.IsZero() {
		next = min(next, assessment.MonitorUntil.UnixNano())
	}
	return time.Unix(0, next)
}

func (s *Store) RunConnectionMonitors(ctx context.Context, evaluate ConnectionEvaluator, logger *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := s.checkConnectionMonitors(ctx, time.Now(), evaluate); err != nil && ctx.Err() == nil {
			logger.Error("connection monitor check failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Store) checkConnectionMonitors(ctx context.Context, now time.Time, evaluate ConnectionEvaluator) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM connection_monitors WHERE due_ns<=? ORDER BY due_ns,id LIMIT ?`, now.UnixNano(), connectionMonitorLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	var due []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		due = append(due, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = errors.Join(err, s.checkConnectionMonitor(ctx, id, now, evaluate))
	}
	return err
}

func (s *Store) checkConnectionMonitor(ctx context.Context, id int64, now time.Time, evaluate ConnectionEvaluator) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var monitor ConnectionMonitor
	var owner string
	var monitorUntil, retire int64
	err = tx.QueryRowContext(ctx, `SELECT owner,inbound,outbound,risk,monitor_until_ns,retire_ns FROM connection_monitors WHERE id=? AND due_ns<=?`, id, now.UnixNano()).
		Scan(&owner, &monitor.Inbound, &monitor.Outbound, &monitor.Risk, &monitorUntil, &retire)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if retire != 0 && retire <= now.UnixNano() {
		return removeConnectionMonitor(ctx, tx, id)
	}
	assessment, err := assessTrackedConnection(ctx, tx, owner, monitor.Inbound, monitor.Outbound, now, evaluate)
	if errors.Is(err, ErrNotFound) {
		return removeConnectionMonitor(ctx, tx, id)
	}
	if err != nil || assessment.Incomplete {
		until := monitorUntil
		if retire != 0 {
			until = retire
		}
		if until <= now.UnixNano() {
			return errors.Join(err, removeConnectionMonitor(ctx, tx, id))
		}
		due := min(until, now.Add(connectionCheckRetry).UnixNano())
		if _, retryErr := tx.ExecContext(ctx, `UPDATE connection_monitors SET due_ns=?,needs_check=1 WHERE id=?`, due, id); retryErr != nil {
			return errors.Join(err, retryErr)
		}
		return errors.Join(err, tx.Commit())
	}
	newRank := slices.Index(connectionRisks[:], assessment.Risk)
	if newRank < 0 {
		return removeConnectionMonitor(ctx, tx, id)
	}
	if assessment.Risk != monitor.Risk {
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE done=0 AND id IN (
SELECT outbox_id FROM connection_monitor_jobs WHERE monitor_id=?)`, id); err != nil {
			return err
		}
		if newRank > slices.Index(connectionRisks[:], monitor.Risk) && assessment.AlertUntil.After(now) {
			if err := enqueueConnectionAlert(ctx, tx, id, owner, monitor, assessment, now); err != nil {
				return err
			}
		}
	}
	due := connectionCheckTime(assessment, now).UnixNano()
	retire = 0
	if assessment.Monitorable && assessment.MonitorUntil.After(now) {
		monitorUntil = assessment.MonitorUntil.UnixNano()
	} else {
		// Keep an ended pair only while its final alert can still be sent.
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(o.expires),0)
FROM connection_monitor_jobs j JOIN outbox o ON o.id=j.outbox_id WHERE j.monitor_id=? AND o.done=0`, id).Scan(&retire); err != nil {
			return err
		}
		if retire <= now.UnixNano() {
			return removeConnectionMonitor(ctx, tx, id)
		}
		due = retire
	}
	if _, err := tx.ExecContext(ctx, `UPDATE connection_monitors SET risk=?,due_ns=?,monitor_until_ns=?,retire_ns=?,needs_check=0 WHERE id=?`,
		assessment.Risk, due, monitorUntil, retire, id); err != nil {
		return err
	}
	return s.commitOutbox(tx)
}

func removeConnectionMonitor(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM connection_monitors WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func enqueueConnectionAlert(ctx context.Context, tx *sql.Tx, id int64, owner string, monitor ConnectionMonitor, assessment ConnectionAssessment, now time.Time) error {
	collapse := hash("connection\x00" + owner + "\x00" + monitor.Inbound + "\x00" + monitor.Outbound)
	msg := push.Message{Kind: push.Alert, FlightID: monitor.Outbound, Title: "Connection risk increased",
		Body:      "Plano estimates that your connection is " + strings.ToLower(assessment.Risk) + ". Check your flight details.",
		Timestamp: now, ExpiresAt: assessment.AlertUntil, CollapseID: collapse}
	if err := enqueue(ctx, tx, monitor.Outbound, msg, `SELECT id FROM devices WHERE owner=? AND kind='alert'`, owner); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO connection_monitor_jobs(monitor_id,outbox_id)
SELECT ?,o.id FROM devices d JOIN outbox o ON o.device_id=d.id
WHERE d.owner=? AND d.kind='alert' AND o.flight_id=? AND o.done=0 AND json_extract(o.message,'$.CollapseID')=?`,
		id, owner, monitor.Outbound, collapse)
	return err
}
