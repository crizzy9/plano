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
	if _, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE platform=? AND token=? AND kind=? AND (owner<>? OR flight_id<>?)`, device.Platform, device.Token, device.Kind, owner, device.FlightID); err != nil {
		return err
	}
	var deviceID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO devices(owner,platform,token,kind,flight_id,registered) VALUES(?,?,?,?,?,?)
ON CONFLICT(platform,token,kind) DO UPDATE SET registered=excluded.registered RETURNING id`, owner, device.Platform, device.Token, device.Kind, device.FlightID, now.UnixNano()).Scan(&deviceID)
	if err != nil {
		return err
	}
	if flight.ID != "" {
		kind := push.LiveUpdate
		if finished(flight) {
			kind = push.LiveEnd
		}
		msg := liveMessage(flight, kind, seat, now)
		if err := queueDevice(ctx, tx, deviceID, flight.ID, msg); err != nil {
			return err
		}
	}
	return tx.Commit()
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
		id                   int64
		platform, kind, seat string
		started              bool
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.id,d.platform,d.kind,t.seat,l.device_id IS NOT NULL FROM devices d JOIN tracked t ON t.owner=d.owner
LEFT JOIN live_started l ON l.device_id=d.id AND l.flight_id=t.flight_id
WHERE t.flight_id=? AND ((d.platform='android' AND d.kind='alert') OR d.kind='live_start' OR (d.kind='live_update' AND d.flight_id=t.flight_id))`, flight.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var recipients []audience
	for rows.Next() {
		var a audience
		if err := rows.Scan(&a.id, &a.platform, &a.kind, &a.seat, &a.started); err != nil {
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
		if err := queueDevice(ctx, tx, a.id, flight.ID, liveMessage(flight, kind, a.seat, now)); err != nil {
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
	rows, err := tx.QueryContext(ctx, `SELECT o.id,o.device_id,d.registered,o.attempts,d.platform,d.token,o.message
FROM outbox o JOIN devices d ON d.id=o.device_id
WHERE o.done=0 AND o.due<=? AND o.expires>? AND o.lease_until<=?
AND (d.platform <> 'android' OR json_extract(o.message,'$.Kind') = 'alert' OR NOT EXISTS (
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
		if err := rows.Scan(&job.id, &job.deviceID, &job.registered, &job.attempts, &job.target.Platform, &job.target.Token, &raw); err != nil {
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
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET attempts=attempts+1,lease_until=0,done=?,due=? WHERE id=? AND lease_until=?`, done, due.UnixNano(), job.id, job.lease); err != nil {
		return err
	}
	return tx.Commit()
}

type PushSender interface {
	Send(context.Context, push.Target, push.Message) error
}

// Dispatch claims small batches durably and keeps remote IO outside transactions.
func (s *Store) Dispatch(ctx context.Context, sender PushSender, logger *slog.Logger) error {
	jobs, err := s.claim(ctx, time.Now(), 16)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Go(func() {
			attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := sender.Send(attempt, job.target, job.message)
			cancel()
			if err != nil {
				logger.Warn("push delivery failed", "job", job.id, "platform", job.target.Platform)
			}
			if err := s.resolve(ctx, job, err, time.Now()); err != nil {
				logger.Error("could not resolve push job", "job", job.id, "error", err)
			}
		})
	}
	wg.Wait()
	return nil
}

func (s *Store) Prune(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM outbox WHERE lease_until<=? AND (expires<=? OR (done=1 AND due<=?))`, now.UnixNano(), now.UnixNano(), now.Add(-24*time.Hour).UnixNano()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM shares WHERE expires<=?`, now.UnixNano())
	return err
}

func (s *Store) RemoveDevice(ctx context.Context, owner string, d Device) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE owner=? AND platform=? AND token=? AND kind=? AND flight_id=?`, owner, d.Platform, d.Token, d.Kind, d.FlightID)
	return err
}

func (s *Store) RemoveDevices(ctx context.Context, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE owner=?`, owner)
	return err
}
