package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

func (s *Store) Register(ctx context.Context, owner string, device Device, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var flight feeds.Flight
	var seat string
	if device.Kind == "live_update" {
		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT f.snapshot,t.seat FROM flights f JOIN tracked t ON t.flight_id=f.id WHERE f.id=? AND t.owner=?`, device.FlightID, owner).Scan(&raw, &seat)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &flight); err != nil {
			return err
		}
	}
	// Token ownership can move when an app is reinstalled. Cascade old queued work.
	if _, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE platform=? AND token=? AND kind=? AND (
owner<>? OR flight_id<>? OR COALESCE((SELECT binding FROM device_bindings b WHERE b.device_id=devices.id),'')<>?)`, device.Platform, device.Token, device.Kind, owner, device.FlightID, device.Binding); err != nil {
		return err
	}
	var deviceID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO devices(owner,platform,token,kind,flight_id,registered) VALUES(?,?,?,?,?,?)
ON CONFLICT(platform,token,kind) DO UPDATE SET registered=excluded.registered RETURNING id`, owner, device.Platform, device.Token, device.Kind, device.FlightID, now.UnixNano()).Scan(&deviceID)
	if err != nil {
		return err
	}
	if device.Binding != "" {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO device_bindings VALUES(?,?)`, deviceID, device.Binding); err != nil {
			return err
		}
	}
	if device.Kind == "live_start" {
		if device.RequestUpdateToken {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO live_token_requests VALUES(?)`, deviceID); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `DELETE FROM live_token_requests WHERE device_id=?`, deviceID); err != nil {
			return err
		}
	}
	if flight.ID != "" {
		// A locally started activity can provide its token before a queued
		// push-to-start is delivered. Suppress that duplicate start.
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE flight_id=? AND done=0 AND lease_until=0
AND json_extract(message,'$.Kind')=? AND device_id IN (SELECT id FROM devices WHERE owner=? AND kind='live_start')`, flight.ID, push.LiveStart, owner); err != nil {
			return err
		}
		kind := push.LiveUpdate
		if finished(flight) {
			kind = push.LiveEnd
		}
		msg := liveMessage(flight, kind, seat, now)
		if err := queueDevice(ctx, tx, deviceID, flight.ID, msg); err != nil {
			return err
		}
	}
	if device.Kind == "alert" {
		rows, err := tx.QueryContext(ctx, `SELECT f.snapshot FROM tracked t JOIN flights f ON f.id=t.flight_id WHERE t.owner=?
AND julianday(COALESCE(json_extract(f.snapshot,'$.estimatedDeparture'),json_extract(f.snapshot,'$.scheduledDeparture'))) BETWEEN julianday(?) AND julianday(?)`, owner, now.Format(time.RFC3339), now.Add(24*time.Hour).Format(time.RFC3339))
		if err != nil {
			return err
		}
		defer rows.Close()
		var flights []feeds.Flight
		for rows.Next() {
			var raw []byte
			var f feeds.Flight
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &f); err != nil {
				return err
			}
			flights = append(flights, f)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, f := range flights {
			if err := queueCheckIn(ctx, tx, f, now); err != nil {
				return err
			}
		}
	}
	return s.commitOutbox(tx)
}

// A flight event can predate an installation's subscription. Track delivery
// separately so later eligible subscribers still receive one check-in reminder.
func queueCheckIn(ctx context.Context, tx *sql.Tx, flight feeds.Flight, now time.Time) error {
	departure := effectiveDeparture(flight)
	if departure == nil || flight.ActualDeparture != nil || finished(flight) || now.Before(departure.Add(-24*time.Hour)) || !now.Before(*departure) {
		return nil
	}
	event := Event{ID: hash(flight.ID + "\x00check_in"), Kind: "check_in", Title: "Check-in reminder", Body: "Check check-in availability for " + flight.Number + " with your airline.", At: now}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO events VALUES(?,?,?,?)`, event.ID, flight.ID, event.At.UnixNano(), string(data)); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `INSERT OR IGNORE INTO check_in_queued(device_id,flight_id)
SELECT d.id,t.flight_id FROM tracked t JOIN devices d ON d.owner=t.owner WHERE t.flight_id=? AND d.kind='alert' RETURNING device_id`, flight.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var recipients []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		recipients = append(recipients, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	expires := now.Add(2 * time.Hour)
	if departure.Before(expires) {
		expires = *departure
	}
	msg := push.Message{Kind: push.Alert, FlightID: flight.ID, Title: event.Title, Body: event.Body, Timestamp: now, ExpiresAt: expires, CollapseID: event.ID}
	for _, id := range recipients {
		if err := queueDevice(ctx, tx, id, flight.ID, msg); err != nil {
			return err
		}
	}
	return nil
}

func liveMessage(flight feeds.Flight, kind push.Kind, seat string, now time.Time) push.Message {
	state := &push.LiveState{FlightID: flight.ID, FlightNumber: flight.Number, Status: flight.Status, Origin: flight.Origin, Destination: flight.Destination, DepartureGate: flight.DepartureGate, ArrivalGate: flight.ArrivalGate, DepartureTerminal: flight.DepartureTerminal, ArrivalTerminal: flight.ArrivalTerminal, Seat: seat, ObservedEpoch: flight.ObservedAt.Unix()}
	if dep := effectiveDeparture(flight); dep != nil {
		state.DepartureEpoch = dep.Unix()
	}
	if arr := effectiveArrival(flight); arr != nil {
		state.ArrivalEpoch = arr.Unix()
	}
	msg := push.Message{Kind: kind, FlightID: flight.ID, Title: flight.Number + " · " + flight.Origin + " to " + flight.Destination, Body: "Track this flight with Plano.", State: state, Timestamp: now, StaleAt: now.Add(10 * time.Minute), ExpiresAt: now.Add(10 * time.Minute), CollapseID: hash(flight.ID)}
	if kind == push.LiveEnd {
		msg.DismissAt = now.Add(20 * time.Minute)
	}
	return msg
}

func enqueueLive(ctx context.Context, tx *sql.Tx, prior, flight feeds.Flight) error {
	departure := effectiveDeparture(flight)
	if departure == nil {
		return nil
	}
	now := flight.ObservedAt
	end := finished(flight)
	withinWindow := !departure.After(now.Add(time.Hour)) && (flight.ActualDeparture != nil || departure.After(now.Add(-2*time.Hour)))
	if !withinWindow && !end {
		return nil
	}
	type audience struct {
		id                          int64
		platform, kind, seat        string
		started, requestUpdateToken bool
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.id,d.platform,d.kind,t.seat,l.device_id IS NOT NULL, q.device_id IS NOT NULL FROM devices d JOIN tracked t ON t.owner=d.owner
LEFT JOIN live_started l ON l.device_id=d.id AND l.flight_id=t.flight_id
LEFT JOIN live_token_requests q ON q.device_id=d.id
WHERE t.flight_id=? AND ((d.platform='android' AND d.kind='alert') OR (d.kind='live_start' AND NOT EXISTS (SELECT 1 FROM devices u WHERE u.owner=d.owner AND u.kind='live_update' AND u.flight_id=t.flight_id)) OR (d.kind='live_update' AND d.flight_id=t.flight_id))`, flight.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var recipients []audience
	for rows.Next() {
		var a audience
		if err := rows.Scan(&a.id, &a.platform, &a.kind, &a.seat, &a.started, &a.requestUpdateToken); err != nil {
			return err
		}
		recipients = append(recipients, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, a := range recipients {
		kind := push.LiveUpdate
		switch {
		case a.kind == "live_start":
			if a.started || end {
				continue
			}
			kind = push.LiveStart
		case end:
			if finished(prior) || a.platform == "android" && !a.started {
				continue
			}
			kind = push.LiveEnd
		case a.platform == "android" && !a.started:
			kind = push.LiveStart
		case prior.ID != "" && now.Sub(prior.ObservedAt) < time.Minute:
			before := liveMessage(prior, kind, a.seat, now)
			after := liveMessage(flight, kind, a.seat, now)
			before.State.ObservedEpoch = after.State.ObservedEpoch
			if *before.State == *after.State {
				continue
			}
		}
		msg := liveMessage(flight, kind, a.seat, now)
		msg.RequestUpdateToken = a.requestUpdateToken && kind == push.LiveStart
		if err := queueDevice(ctx, tx, a.id, flight.ID, msg); err != nil {
			return err
		}
		if kind == push.LiveStart {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO live_started VALUES(?,?)`, a.id, flight.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func queueDevice(ctx context.Context, tx *sql.Tx, deviceID int64, flightID string, msg push.Message) error {
	if msg.Kind == push.LiveUpdate || msg.Kind == push.LiveEnd {
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE device_id=? AND flight_id=? AND done=0 AND lease_until=0 AND json_extract(message,'$.Kind')=?`, deviceID, flightID, push.LiveUpdate); err != nil {
			return err
		}
	}
	if msg.Kind != push.Alert {
		// ActivityKit orders updates by whole Unix seconds. Keep the wire clock
		// per token and flight, even across process restarts.
		var epoch int64
		err := tx.QueryRowContext(ctx, `INSERT INTO live_wire_clocks(device_id,flight_id,wire_epoch)
SELECT id,?,? FROM devices WHERE id=? AND platform='ios'
ON CONFLICT(device_id,flight_id) DO UPDATE SET wire_epoch=MAX(excluded.wire_epoch,live_wire_clocks.wire_epoch+1)
RETURNING wire_epoch`, flightID, msg.Timestamp.Unix(), deviceID).Scan(&epoch)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && epoch > msg.Timestamp.Unix() {
			msg.Timestamp = time.Unix(epoch, 0).UTC()
		}
		if !msg.Timestamp.Before(msg.ExpiresAt) {
			return nil
		}
	}
	return enqueue(ctx, tx, flightID, msg, `SELECT ? AS id`, deviceID)
}

func enqueue(ctx context.Context, tx *sql.Tx, flightID string, msg push.Message, recipientsSQL string, args ...any) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	values := append([]any{flightID, string(raw), msg.Timestamp.UnixNano(), msg.ExpiresAt.UnixNano()}, args...)
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox(device_id,flight_id,message,due,expires) SELECT id,?,?,?,? FROM (`+recipientsSQL+`) AS recipients`, values...)
	return err
}

type deliveryJob struct {
	id, deviceID, registered, lease int64
	attempts                        int
	target                          push.Target
	message                         push.Message
}

func (s *Store) claim(ctx context.Context, now time.Time, limit int) ([]deliveryJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t := now.UnixNano()
	rows, err := tx.QueryContext(ctx, `SELECT o.id,o.device_id,d.registered,o.attempts,d.platform,d.token,COALESCE(b.binding,''),o.message
FROM outbox o JOIN devices d ON d.id=o.device_id LEFT JOIN device_bindings b ON b.device_id=d.id
WHERE o.done=0 AND o.due<=? AND o.expires>? AND o.lease_until<=?
AND (json_extract(o.message,'$.Kind') = 'alert' OR NOT EXISTS (
  SELECT 1 FROM outbox older WHERE older.device_id=o.device_id AND older.flight_id=o.flight_id AND older.id<o.id
  AND older.done=0 AND older.expires>? AND json_extract(older.message,'$.Kind') <> 'alert'
)) ORDER BY o.due,o.id LIMIT ?`, t, t, t, t, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []deliveryJob
	for rows.Next() {
		var job deliveryJob
		var raw []byte
		if err := rows.Scan(&job.id, &job.deviceID, &job.registered, &job.attempts, &job.target.Platform, &job.target.Token, &job.target.Binding, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &job.message); err != nil {
			return nil, err
		}
		job.lease = now.Add(30 * time.Second).UnixNano()
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, job := range jobs {
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET lease_until=? WHERE id=?`, job.lease, job.id); err != nil {
			return nil, err
		}
	}
	return jobs, tx.Commit()
}

func (s *Store) resolve(ctx context.Context, job deliveryJob, sent error, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var delivery *push.DeliveryError
	if errors.As(sent, &delivery) && delivery.InvalidToken && (delivery.TokenInvalidatedAt.IsZero() || delivery.TokenInvalidatedAt.UnixNano() >= job.registered) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE id=? AND registered=?`, job.deviceID, job.registered); err != nil {
			return err
		}
	}
	done := true
	due := now
	if (delivery != nil && delivery.Retryable || errors.Is(sent, context.DeadlineExceeded)) && job.attempts < 10 {
		retryAfter := time.Duration(0)
		if delivery != nil {
			retryAfter = delivery.RetryAfter
		}
		due = now.Add(max(time.Second<<job.attempts, retryAfter))
		done = !due.Before(job.message.ExpiresAt)
	}
	result, err := tx.ExecContext(ctx, `UPDATE outbox SET attempts=attempts+1,lease_until=0,done=?,due=? WHERE id=? AND lease_until=?`, done, due.UnixNano(), job.id, job.lease)
	if err != nil {
		return err
	}
	if done && sent != nil && job.message.Kind == push.LiveStart {
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if updated == 1 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM live_started WHERE device_id=? AND flight_id=?`, job.deviceID, job.message.FlightID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

type PushSender interface {
	Send(context.Context, push.Target, push.Message) error
}

// commitOutbox publishes a coalescing wake only after SQLite has committed.
func (s *Store) commitOutbox(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	select {
	case s.outboxWake <- struct{}{}:
	default:
	}
	return nil
}

// RunDispatcher keeps independent sends in flight. A slow provider request
// cannot stop ready work while a slot is available. The tick also recovers
// persisted jobs and retries if a process-local wake was lost.
func (s *Store) RunDispatcher(ctx context.Context, sender PushSender, logger *slog.Logger) {
	const slots = 16
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	completed := make(chan struct{}, slots)
	var workers sync.WaitGroup
	defer workers.Wait()
	busy := 0
	for ctx.Err() == nil {
		if busy < slots {
			jobs, err := s.claim(ctx, time.Now(), slots-busy)
			if err != nil && ctx.Err() == nil {
				logger.Error("push dispatcher failed", "error", err)
			}
			for _, job := range jobs {
				busy++
				workers.Go(func() {
					defer func() { completed <- struct{}{} }()
					s.deliver(ctx, sender, logger, job)
				})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-completed:
			busy--
		case <-s.outboxWake:
		case <-tick.C:
		}
	}
}

func (s *Store) deliver(ctx context.Context, sender PushSender, logger *slog.Logger, job deliveryJob) {
	start := time.Now()
	attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := sender.Send(attempt, job.target, job.message)
	cancel()
	if err != nil {
		logger.Warn("push delivery failed", "job", job.id, "platform", job.target.Platform)
	} else {
		logger.Debug("push accepted by provider", "job", job.id, "platform", job.target.Platform, "request_ms", time.Since(start).Milliseconds(), "observation_to_send_ms", start.Sub(job.message.Timestamp).Milliseconds())
	}
	if err := s.resolve(ctx, job, err, time.Now()); err != nil && ctx.Err() == nil {
		logger.Error("could not resolve push job", "job", job.id, "error", err)
	}
}

func (s *Store) Prune(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM live_started WHERE (device_id,flight_id) IN (SELECT device_id,flight_id FROM outbox WHERE lease_until<=? AND expires<=? AND done=0 AND json_extract(message,'$.Kind')=?)`, now.UnixNano(), now.UnixNano(), push.LiveStart); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE lease_until<=? AND (expires<=? OR (done=1 AND due<=?))`, now.UnixNano(), now.UnixNano(), now.Add(-24*time.Hour).UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM shares WHERE expires<=?`, now.UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pushed_advisories WHERE expires_ns<=? AND observed_ns<=?`, now.UnixNano(), now.Add(-7*time.Hour).UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM live_wire_clocks WHERE wire_epoch<=?`, now.Add(-7*24*time.Hour).Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM radar_positions WHERE at_ns<=?`, now.Add(-7*24*time.Hour).UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM friend_invites WHERE expires_ns<=?`, now.UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveDevice(ctx context.Context, owner string, d Device) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE owner=? AND platform=? AND kind=? AND flight_id=?
AND (token=? OR (?='' AND ?<>''))
AND (?='' OR EXISTS(SELECT 1 FROM device_bindings WHERE device_id=devices.id AND binding=?))`, owner, d.Platform, d.Kind, d.FlightID, d.Token, d.Token, d.Binding, d.Binding, d.Binding)
	return err
}

func (s *Store) RemoveDevices(ctx context.Context, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE owner=?`, owner)
	return err
}
