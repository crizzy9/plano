package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

var (
	ErrInvalidLiveSlot = errors.New("invalid Live Activity registration slot")
	activityLiveSlot   = regexp.MustCompile(`^activity:[0-9a-f]{64}$`)
	liveSessionBinding = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

func (s *Store) Register(ctx context.Context, owner string, device Device, now time.Time) error {
	if device.LiveSlot == "" && device.LiveRevision != 0 &&
		(device.LiveRevision < 0 || device.Platform != "ios" || device.Kind != "alert" || device.FlightID != "" || device.Binding != "") ||
		device.LiveSlot != "" && (device.LiveRevision <= 0 || device.Platform != "ios" ||
			!(device.Kind == "live_start" && device.LiveSlot == "start" && device.FlightID == "" ||
				device.Kind == "live_update" && device.FlightID != "" && activityLiveSlot.MatchString(device.LiveSlot))) ||
		device.Platform == "ios" && device.Binding != "" && (device.LiveSlot == "" || !liveSessionBinding.MatchString(device.Binding)) {
		return ErrInvalidLiveSlot
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if device.Platform == "ios" {
		// Check the installation floor before a late request can take ownership
		// of the same token from a different installation.
		var ignored bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM ios_device_floors WHERE owner=? AND revision>=?
) OR EXISTS(
SELECT 1 FROM devices d JOIN ios_alert_revisions r ON d.id=r.device_id
WHERE d.owner=? AND d.platform='ios' AND d.kind=? AND d.token=? AND r.revision>=?
)`, owner, device.LiveRevision, owner, device.Kind, device.Token, device.LiveRevision).Scan(&ignored); err != nil {
			return err
		}
		if ignored {
			return nil
		}
		if device.Kind == "live_start" && device.LiveSlot == "" {
			// A request from before slot registration cannot take a bound token
			// back from a newer client, including after its delivery row is gone.
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM ios_live_start_bindings WHERE owner=? AND binding<>''
) OR EXISTS(
SELECT 1 FROM devices d JOIN device_bindings b ON b.device_id=d.id
WHERE d.platform='ios' AND d.kind='live_start' AND d.token=? AND b.binding<>''
)`, owner, device.Token).Scan(&ignored); err != nil || ignored {
				return err
			}
		}
	}
	var priorDeviceID int64
	if device.LiveSlot != "" {
		var ignored bool
		priorDeviceID, ignored, err = priorLiveSlotDevice(ctx, tx, owner, device)
		if err != nil || ignored {
			return err
		}
	}
	var flight feeds.Flight
	var seat string
	var pendingImportantUpdate bool
	if device.Kind == "live_update" {
		// An update from a prior session can finish after the new start slot.
		// A newer local activity may register before its own start token exists.
		var retired bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM device_live_slots l JOIN ios_live_start_bindings b USING(owner)
WHERE l.owner=? AND l.slot='start' AND l.revision>=? AND b.binding<>?)`,
			owner, device.LiveRevision, device.Binding).Scan(&retired); err != nil || retired {
			return err
		}
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
		if priorDeviceID != 0 {
			// A rotation can retire the old device and cascade its outbox.
			// Keep pending urgency only for the same activity and binding.
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM outbox o LEFT JOIN device_bindings b ON b.device_id=o.device_id
WHERE o.device_id=? AND o.flight_id=? AND COALESCE(b.binding,'')=? AND o.done=0 AND o.expires>?
AND json_extract(o.message,'$.APNsImmediateLiveUpdate')=1)`,
				priorDeviceID, flight.ID, device.Binding, now.UnixNano()).Scan(&pendingImportantUpdate); err != nil {
				return err
			}
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
	if device.LiveSlot != "" {
		if err := bindLiveSlot(ctx, tx, owner, device, deviceID, priorDeviceID); err != nil {
			return err
		}
	} else if device.LiveRevision > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ios_alert_revisions(device_id,revision) VALUES(?,?)
ON CONFLICT(device_id) DO UPDATE SET revision=excluded.revision`, deviceID, device.LiveRevision); err != nil {
			return err
		}
	}
	if device.Binding != "" {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO device_bindings VALUES(?,?)`, deviceID, device.Binding); err != nil {
			return err
		}
	}
	if device.Kind == "live_start" {
		if device.LiveSlot == "start" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO ios_live_start_bindings VALUES(?,?)
ON CONFLICT(owner) DO UPDATE SET binding=excluded.binding`, owner, device.Binding); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM ios_live_start_submissions WHERE owner=? AND binding<>?`, owner, device.Binding); err != nil {
				return err
			}
		}
		if device.Binding != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM devices AS d WHERE d.owner=? AND d.platform='ios' AND d.kind IN ('live_start','live_update')
AND COALESCE((SELECT binding FROM device_bindings WHERE device_id=d.id),'')<>?
AND COALESCE((SELECT revision FROM device_live_slots WHERE device_id=d.id),0)<?`, owner, device.Binding, device.LiveRevision); err != nil {
				return err
			}
		}
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
AND json_extract(message,'$.Kind')=? AND device_id IN (SELECT d.id FROM devices d LEFT JOIN device_bindings b ON b.device_id=d.id
WHERE d.owner=? AND d.kind='live_start' AND COALESCE(b.binding,'')=?)`, flight.ID, push.LiveStart, owner, device.Binding); err != nil {
			return err
		}
		kind := push.LiveUpdate
		if finished(flight) {
			kind = push.LiveEnd
		}
		if kind == push.LiveUpdate && pendingImportantUpdate && deviceID == priorDeviceID {
			return s.commitOutbox(tx)
		}
		msg := liveMessage(flight, kind, seat, now)
		msg.APNsImmediateLiveUpdate = kind == push.LiveUpdate && pendingImportantUpdate
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

// A slot's receipt survives token deletion so a delayed registration cannot
// reactivate a retired token. An equal retry never enqueues a second update.
func priorLiveSlotDevice(ctx context.Context, tx *sql.Tx, owner string, device Device) (int64, bool, error) {
	var prior sql.NullInt64
	var revision int64
	var flight, token, binding string
	err := tx.QueryRowContext(ctx, `SELECT l.device_id,l.flight_id,l.revision,COALESCE(d.token,''),COALESCE(b.binding,'')
FROM device_live_slots l LEFT JOIN devices d ON d.id=l.device_id LEFT JOIN device_bindings b ON b.device_id=d.id
WHERE l.owner=? AND l.slot=?`, owner, device.LiveSlot).Scan(&prior, &flight, &revision, &token, &binding)
	if err == nil {
		if device.LiveRevision < revision {
			return 0, true, nil
		}
		if flight != device.FlightID {
			return 0, false, ErrInvalidLiveSlot
		}
		if device.LiveRevision == revision {
			if prior.Valid && (token != device.Token || binding != device.Binding) {
				return 0, false, ErrInvalidLiveSlot
			}
			return 0, true, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	// The incoming token may belong to another installation after a reinstall.
	// Within this installation it must belong to the same flight and slot.
	var conflict bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
  SELECT 1 FROM devices d LEFT JOIN device_live_slots l ON l.device_id=d.id
  WHERE d.platform=? AND d.token=? AND d.kind=? AND d.owner=?
  AND (d.flight_id<>? OR (l.slot IS NOT NULL AND l.slot<>?))
)`, device.Platform, device.Token, device.Kind, owner, device.FlightID, device.LiveSlot).Scan(&conflict); err != nil {
		return 0, false, err
	}
	if conflict {
		return 0, false, ErrInvalidLiveSlot
	}
	return prior.Int64, false, nil
}

func bindLiveSlot(ctx context.Context, tx *sql.Tx, owner string, device Device, deviceID, priorID int64) error {
	if priorID != 0 && priorID != deviceID {
		// ActivityKit orders updates to the same activity by whole seconds,
		// including when its token changes during that second.
		if _, err := tx.ExecContext(ctx, `INSERT INTO live_wire_clocks(device_id,flight_id,wire_epoch)
SELECT ?,flight_id,wire_epoch FROM live_wire_clocks WHERE device_id=?
ON CONFLICT(device_id,flight_id) DO UPDATE SET wire_epoch=MAX(live_wire_clocks.wire_epoch,excluded.wire_epoch)`, deviceID, priorID); err != nil {
			return err
		}
		if device.Kind == "live_start" {
			var sameBinding bool
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT binding FROM device_bindings WHERE device_id=?),'')=?`,
				priorID, device.Binding).Scan(&sameBinding); err != nil {
				return err
			}
			if sameBinding {
				if err := carryConfirmedStarts(ctx, tx, deviceID, priorID); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE id=?`, priorID); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO device_live_slots(owner,slot,flight_id,revision,device_id) VALUES(?,?,?,?,?)
ON CONFLICT(owner,slot) DO UPDATE SET revision=excluded.revision,device_id=excluded.device_id`,
		owner, device.LiveSlot, device.FlightID, device.LiveRevision, deviceID)
	return err
}

func carryConfirmedStarts(ctx context.Context, tx *sql.Tx, deviceID, priorID int64) error {
	// Preserve legacy device-scoped receipts on a slot rotation. Bound
	// submissions also survive independently of the device row.
	const confirmed = `SELECT ?,flight_id FROM live_start_confirmations WHERE device_id=?`
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO live_started `+confirmed, deviceID, priorID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO live_start_confirmations `+confirmed, deviceID, priorID)
	return err
}

// A flight event can predate an installation's subscription. Track delivery
// separately so later eligible subscribers still receive one check-in reminder.
func queueCheckIn(ctx context.Context, tx *sql.Tx, flight feeds.Flight, now time.Time) error {
	departure := checkInDeadline(flight, now)
	if departure == nil {
		return nil
	}
	event := Event{ID: hash(flight.ID + "\x00check_in"), Kind: "check_in", Title: "Check-in reminder", Body: "Check check-in availability for " + flight.Number + " with your airline.", At: now}
	if _, err := insertFlightEvent(ctx, tx, flight.ID, &event); err != nil {
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

const (
	liveFreshness         = 10 * time.Minute
	liveHeartbeatInterval = time.Minute
)

func liveContentState(flight feeds.Flight, seat string) *push.LiveState {
	state := &push.LiveState{FlightID: flight.ID, FlightNumber: flight.Number, Status: flight.Status, Origin: flight.Origin, Destination: flight.Destination, DepartureGate: flight.DepartureGate, ArrivalGate: flight.ArrivalGate, DepartureTerminal: flight.DepartureTerminal, ArrivalTerminal: flight.ArrivalTerminal, Seat: seat, ObservedEpoch: flight.ObservedAt.Unix()}
	if dep := firstTime(flight.ActualDeparture, flight.ActualTakeoff, flight.EstimatedDeparture, flight.ScheduledDeparture); dep != nil {
		state.DepartureEpoch = dep.Unix()
	}
	if flight.Status == "diverted" {
		// The provider can retain the original destination and its schedule.
		state.ArrivalGate, state.ArrivalTerminal = "", ""
	} else if arr := effectiveArrival(flight); arr != nil {
		state.ArrivalEpoch = arr.Unix()
	}
	return state
}

func liveMessage(flight feeds.Flight, kind push.Kind, seat string, now time.Time) push.Message {
	state := liveContentState(flight, seat)
	staleAt := now
	if state.ObservedEpoch > 0 {
		observed := now
		if flight.ObservedAt.Before(now) {
			observed = flight.ObservedAt
		}
		staleAt = observed.Add(liveFreshness)
	}
	title := flight.Number + " · " + flight.Origin + " to " + flight.Destination
	if flight.Status == "diverted" {
		title = "Flight diverted"
		if flight.Number != "" {
			title = flight.Number + " · " + title
		}
	}
	msg := push.Message{Kind: kind, FlightID: flight.ID, Title: title, Body: "Track this flight with Plano.", State: state, Timestamp: now, StaleAt: staleAt, ExpiresAt: now.Add(liveFreshness), CollapseID: hash(flight.ID)}
	if observedNs := flight.ObservedAt.UnixNano(); state.ObservedEpoch > 0 && observedNs > 0 &&
		time.Unix(0, observedNs).Equal(flight.ObservedAt) {
		msg.LiveObservedNs = observedNs
	}
	if kind == push.LiveEnd {
		msg.DismissAt = now.Add(20 * time.Minute)
	}
	return msg
}

func importantLiveChange(before, after *push.LiveState) bool {
	if before.Status != after.Status {
		switch after.Status {
		case "boarding", "taxi_out", "airborne", "landed", "diverted":
			return true
		case "scheduled":
			if before.Status != "" && before.Status != "unknown" {
				return true
			}
		}
	}
	for _, location := range [][2]string{
		{before.DepartureGate, after.DepartureGate},
		{before.DepartureTerminal, after.DepartureTerminal},
		{before.ArrivalGate, after.ArrivalGate},
		{before.ArrivalTerminal, after.ArrivalTerminal},
	} {
		if !strings.EqualFold(strings.TrimSpace(location[0]), strings.TrimSpace(location[1])) {
			return true
		}
	}
	return false
}

func inLiveStartWindow(flight feeds.Flight, now time.Time) bool {
	if !flightBeforeDeparture(flight) {
		return true
	}
	departure := effectiveDeparture(flight)
	return departure != nil && !departure.After(now.Add(time.Hour)) && departure.After(now.Add(-2*time.Hour))
}

func enqueueLive(ctx context.Context, tx *sql.Tx, prior, flight feeds.Flight, remapped bool) error {
	now := flight.ObservedAt
	end := finished(flight)
	canStart := inLiveStartWindow(flight, now)
	if !canStart {
		// Avoid scanning the device audience for untracked schedule results.
		var tracked bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tracked WHERE flight_id=?)`, flight.ID).Scan(&tracked)
		if err != nil || !tracked {
			return err
		}
	}
	type audience struct {
		id                          int64
		platform, kind, seat        string
		started, requestUpdateToken bool
		recentMessage               []byte
		recentPending               bool
	}
	// Live pushes expire at a fixed offset from enqueue time. Unlike due (retry
	// time) and the APNs wire timestamp, expiry preserves that offset.
	recentExpiry := now.Add(liveFreshness - liveHeartbeatInterval)
	// Android records a start when it queues it. Outside the start window,
	// wait for that job to settle before treating the recipient as active.
	rows, err := tx.QueryContext(ctx, `SELECT d.id,d.platform,d.kind,t.seat,l.device_id IS NOT NULL OR EXISTS(
SELECT 1 FROM ios_live_start_submissions a WHERE d.kind='live_start' AND a.owner=d.owner AND a.flight_id=t.flight_id AND a.binding=b.binding
), q.device_id IS NOT NULL,
recent.message, COALESCE(recent.done=0,0)
FROM devices d JOIN tracked t ON t.owner=d.owner
LEFT JOIN device_bindings b ON b.device_id=d.id
LEFT JOIN live_started l ON l.device_id=d.id AND l.flight_id=t.flight_id
LEFT JOIN live_token_requests q ON q.device_id=d.id
LEFT JOIN outbox recent ON NOT ? AND recent.id=(SELECT o.id FROM outbox o
WHERE o.device_id=d.id AND o.flight_id=t.flight_id AND o.expires>?
AND (json_extract(o.message,'$.Kind') IN ('start','update') OR d.platform='android' AND json_extract(o.message,'$.Kind')='end')
ORDER BY o.id DESC LIMIT 1)
WHERE t.flight_id=? AND ((d.platform='android' AND d.kind='alert') OR (d.kind='live_start' AND NOT EXISTS (
  SELECT 1 FROM devices u LEFT JOIN device_bindings ub ON ub.device_id=u.id
  WHERE u.owner=d.owner AND u.kind='live_update' AND u.flight_id=t.flight_id AND COALESCE(ub.binding,'')=COALESCE(b.binding,'')
)) OR (d.kind='live_update' AND d.flight_id=t.flight_id))
AND (? OR d.platform<>'android' OR NOT EXISTS(
  SELECT 1 FROM outbox queued WHERE queued.device_id=d.id AND queued.flight_id=t.flight_id
  AND queued.done=0 AND json_extract(queued.message,'$.Kind')='start'
))`, remapped, now.UnixNano(), flight.ID, canStart)
	if err != nil {
		return err
	}
	defer rows.Close()
	var recipients []audience
	for rows.Next() {
		var a audience
		if err := rows.Scan(&a.id, &a.platform, &a.kind, &a.seat, &a.started, &a.requestUpdateToken, &a.recentMessage, &a.recentPending); err != nil {
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
	previous := liveContentState(prior, "")
	for _, a := range recipients {
		kind := push.LiveUpdate
		switch {
		case a.kind == "live_start":
			if a.started || end {
				continue
			}
			kind = push.LiveStart
		case end:
			if !remapped && finished(prior) || a.platform == "android" && !a.started {
				continue
			}
			kind = push.LiveEnd
		case a.platform == "android" && !a.started:
			kind = push.LiveStart
		}
		// An active surface still needs updates when a delay moves the
		// flight outside the window for new starts.
		if kind == push.LiveStart && !canStart {
			continue
		}
		msg := liveMessage(flight, kind, a.seat, now)
		if kind == push.LiveUpdate {
			var recent push.Message
			if len(a.recentMessage) > 0 {
				if err := json.Unmarshal(a.recentMessage, &recent); err != nil {
					return err
				}
			}
			if a.platform == "ios" {
				baseline := previous
				if recent.State != nil {
					baseline = recent.State
				}
				msg.APNsImmediateLiveUpdate = importantLiveChange(baseline, msg.State)
				if a.recentPending && recent.APNsImmediateLiveUpdate && !msg.APNsImmediateLiveUpdate {
					// Routine refreshes must not replace a pending important
					// change or reset its provider retry deadline.
					continue
				}
			}
			// A token registered from cached data may have queued stale state.
			// Let the first fresh observation recover the surface immediately.
			if recent.Kind != push.LiveEnd && recent.State != nil && recent.State.ObservedEpoch > now.Add(-liveFreshness).Unix() {
				recent.State.ObservedEpoch = msg.State.ObservedEpoch
				if *recent.State == *msg.State {
					if recent.ExpiresAt.After(recentExpiry) || a.platform == "android" && a.recentPending && !recent.AndroidLiveHeartbeat {
						// A best-effort heartbeat must not replace a queued change.
						continue
					}
					msg.AndroidLiveHeartbeat = a.platform == "android"
				}
			}
		}
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
	return enqueueWithAdvisories(ctx, tx, flightID, msg, nil, recipientsSQL, args...)
}

func enqueueWithAdvisories(ctx context.Context, tx *sql.Tx, flightID string, msg push.Message, evidence []forecastAdvisory, recipientsSQL string, args ...any) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	values := append([]any{flightID, string(raw), msg.Timestamp.UnixNano(), msg.ExpiresAt.UnixNano()}, args...)
	query := `INSERT INTO outbox(device_id,flight_id,message,due,expires) SELECT id,?,?,?,? FROM (` + recipientsSQL + `) AS recipients`
	if len(evidence) == 0 {
		_, err := tx.ExecContext(ctx, query, values...)
		return err
	}
	jobs, err := tx.QueryContext(ctx, query+` RETURNING id`, values...)
	if err != nil {
		return err
	}
	jobIDs, err := readOutboxJobIDs(jobs)
	if err != nil || len(jobIDs) == 0 {
		return err
	}
	return linkOutboxAdvisories(ctx, tx, jobIDs, evidence)
}

type deliveryJob struct {
	id, deviceID, registered, lease int64
	attempts                        int
	checkIn                         bool
	owner                           string
	target                          push.Target
	message                         push.Message
}

func (s *Store) claim(ctx context.Context, now time.Time, limit int) ([]deliveryJob, error) {
	return s.claimForPlatform(ctx, now, limit, "")
}

func (s *Store) claimForPlatform(ctx context.Context, now time.Time, limit int, platform string) ([]deliveryJob, error) {
	return s.claimReadyPush(ctx, now, limit, platform, false)
}

func (s *Store) claimReadyPush(ctx context.Context, now time.Time, limit int, platform string, alertsOnly bool) ([]deliveryJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t := now.UnixNano()
	args := []any{t, t, t}
	filter := ""
	deviceJoin := "JOIN"
	if platform != "" {
		filter = " AND d.platform=?"
		args = append(args, platform)
	}
	if alertsOnly {
		filter += " AND " + pendingAlertKind
		// Walk the ready-alert index in due order before looking up devices.
		deviceJoin = "CROSS JOIN"
	}
	args = append(args, t, limit)
	rows, err := tx.QueryContext(ctx, `SELECT o.id,o.device_id,d.registered,o.attempts,d.owner,d.platform,d.token,COALESCE(b.binding,''),o.message,
COALESCE(json_extract(e.data,'$.kind')='check_in',0)
FROM outbox o `+deviceJoin+` devices d ON d.id=o.device_id LEFT JOIN device_bindings b ON b.device_id=d.id`+outboxFlightEvent+`
WHERE o.done=0 AND o.due<=? AND o.expires>? AND o.lease_until<=?`+filter+` AND NOT `+dirtyConnectionAlert+`
AND NOT `+revokedFriendAlert+`
AND (json_extract(o.message,'$.Kind') = 'alert' OR NOT EXISTS (
  SELECT 1 FROM outbox older WHERE older.device_id=o.device_id AND older.flight_id=o.flight_id AND older.id<o.id
  AND older.done=0 AND older.expires>? AND json_extract(older.message,'$.Kind') <> 'alert'
)) ORDER BY o.due,o.id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []deliveryJob
	for rows.Next() {
		var job deliveryJob
		var raw []byte
		if err := rows.Scan(&job.id, &job.deviceID, &job.registered, &job.attempts, &job.owner,
			&job.target.Platform, &job.target.Token, &job.target.Binding, &raw, &job.checkIn); err != nil {
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
		if job.message.Kind == push.LiveStart {
			// A lease from before submission tracking may already have sent.
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox_live_start_leases
SELECT id,0 FROM outbox WHERE id=? AND lease_until=0 AND attempts=0`, job.id); err != nil {
				return nil, err
			}
		}
		if job.checkIn {
			// Carry an unsent marker forward only when it describes the old
			// lease, or when the job has no active lease and no known send.
			if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_check_in_leases(outbox_id,lease_until,may_have_sent)
SELECT o.id,?,0 FROM outbox o LEFT JOIN outbox_check_in_leases l ON l.outbox_id=o.id
WHERE o.id=? AND o.attempts=0 AND `+unsentCheckInLease+`
ON CONFLICT(outbox_id) DO UPDATE SET lease_until=excluded.lease_until`, job.lease, job.id); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET lease_until=? WHERE id=?`, job.lease, job.id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Store) resolve(ctx context.Context, job deliveryJob, sent error, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var delivery *push.DeliveryError
	_ = errors.As(sent, &delivery)
	unknownStart := job.message.Kind == push.LiveStart && job.target.Platform == "ios" &&
		(errors.Is(sent, context.DeadlineExceeded) || errors.Is(sent, context.Canceled) ||
			delivery != nil && delivery.Status == 0 && delivery.Code == "TransportError")
	done := true
	due := now
	if !unknownStart && (delivery != nil && delivery.Retryable || errors.Is(sent, context.DeadlineExceeded)) && job.attempts < 10 {
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
	if job.message.Kind == push.LiveStart {
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if err := resolveLiveStart(ctx, tx, job, updated == 1, done, sent == nil, unknownStart); err != nil {
			return err
		}
	}
	if delivery != nil && delivery.InvalidToken && (delivery.TokenInvalidatedAt.IsZero() || delivery.TokenInvalidatedAt.UnixNano() >= job.registered) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE id=? AND registered=?`, job.deviceID, job.registered); err != nil {
			return err
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

func dispatcherPlatforms(sender PushSender) []string {
	if pointer, ok := sender.(*push.Sender); ok {
		if pointer == nil {
			return nil
		}
		sender = *pointer
	}
	if configured, ok := sender.(push.Sender); ok {
		var platforms []string
		if configured.APNs != nil {
			platforms = append(platforms, "ios")
		}
		if configured.FCM != nil {
			platforms = append(platforms, "android")
		}
		return platforms
	}
	return []string{"ios", "android"}
}

const (
	dispatchPollInterval = 200 * time.Millisecond
	pendingAlertKind     = `json_extract(message,'$.Kind')='alert'`
)

// RunDispatcher reserves capacity for each provider and for passenger alerts
// within each provider. The tick recovers persisted jobs and retries.
func (s *Store) RunDispatcher(ctx context.Context, sender PushSender, logger *slog.Logger) {
	const slots, reservedAlerts = 16, 2
	platforms := dispatcherPlatforms(sender)
	if len(platforms) == 0 {
		return
	}
	perPlatform := slots / len(platforms)
	type sendLane struct {
		platform    string
		alertsOnly  bool
		limit, busy int
	}
	lanes := make([]sendLane, 0, len(platforms)*2)
	for _, platform := range platforms {
		lanes = append(lanes, sendLane{platform: platform, alertsOnly: true, limit: reservedAlerts})
	}
	for _, platform := range platforms {
		lanes = append(lanes, sendLane{platform: platform, limit: perPlatform - reservedAlerts})
	}
	tick := time.NewTicker(dispatchPollInterval)
	defer tick.Stop()
	completed := make(chan int, slots)
	var workers sync.WaitGroup
	defer workers.Wait()
	workers.Go(func() { s.runCheckInScheduler(ctx, logger) })
	for ctx.Err() == nil {
		for lane := range lanes {
			pool := &lanes[lane]
			available := pool.limit - pool.busy
			if available == 0 {
				continue
			}
			jobs, err := s.claimReadyPush(ctx, time.Now(), available, pool.platform, pool.alertsOnly)
			if err != nil && ctx.Err() == nil {
				logger.Error("push dispatcher failed", "platform", pool.platform, "error", err)
			}
			for _, job := range jobs {
				pool.busy++
				workers.Go(func() {
					defer func() { completed <- lane }()
					s.deliver(ctx, sender, logger, job)
				})
			}
		}
		select {
		case <-ctx.Done():
			return
		case lane := <-completed:
			lanes[lane].busy--
		case <-s.outboxWake:
		case <-tick.C:
		}
	}
}

func (s *Store) deliver(ctx context.Context, sender PushSender, logger *slog.Logger, job deliveryJob) {
	// Revoking a device or untracking a flight can remove a leased row. Check
	// the lease again at the transport boundary without holding a transaction.
	var expires int64
	var connectionDirty, hasAdvisoryEvidence, friendAlert bool
	var flightEventKind, flightPhase string
	var latestFlight, latestForecast, latestRotation []byte
	var newerFlightEvent bool
	const leaseFilter = ` WHERE o.id=? AND o.device_id=? AND o.lease_until=? AND o.done=0`
	const leasedRow = ` FROM outbox o` + leaseFilter
	err := s.db.QueryRowContext(ctx, `SELECT o.expires, `+dirtyConnectionAlert+`,
EXISTS(SELECT 1 FROM outbox_advisory_evidence WHERE outbox_id=o.id),
EXISTS(SELECT 1 FROM outbox_friend_jobs WHERE outbox_id=o.id),
COALESCE(json_extract(e.data,'$.kind'),''), COALESCE(json_extract(f.snapshot,'$.status'),''), f.snapshot,
CASE WHEN json_extract(e.data,'$.kind')='delay_prediction' THEN f.prediction END,
CASE WHEN json_extract(e.data,'$.kind')='delay_prediction' THEN f.inbound END,
EXISTS(SELECT 1 FROM events newer WHERE newer.flight_id=e.flight_id AND newer.at_ns>e.at_ns
  AND json_extract(newer.data,'$.kind')=json_extract(e.data,'$.kind')
  AND COALESCE((SELECT revision FROM event_revisions WHERE event_id=newer.id),0)=
    COALESCE((SELECT revision FROM event_revisions WHERE event_id=e.id),0)
  AND EXISTS(SELECT 1 FROM outbox replacement WHERE replacement.device_id=o.device_id AND replacement.flight_id=o.flight_id
    AND json_extract(replacement.message,'$.CollapseID')=newer.id))
FROM outbox o`+outboxFlightEvent+` LEFT JOIN flights f ON f.id=e.flight_id`+leaseFilter,
		job.id, job.deviceID, job.lease).Scan(&expires, &connectionDirty, &hasAdvisoryEvidence, &friendAlert,
		&flightEventKind, &flightPhase, &latestFlight, &latestForecast, &latestRotation, &newerFlightEvent)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
			logger.Error("could not validate push lease", "job", job.id, "error", err)
		}
		return
	}
	if connectionDirty {
		// Re-evaluation can keep the same warning. Return its lease without
		// consuming a delivery attempt so it can be claimed again promptly.
		if _, err := s.db.ExecContext(ctx, `UPDATE outbox SET lease_until=0 WHERE id=? AND device_id=? AND lease_until=? AND done=0`,
			job.id, job.deviceID, job.lease); err != nil && ctx.Err() == nil {
			logger.Error("could not release connection push lease", "job", job.id, "error", err)
		}
		return
	}
	stateAlert := flightAlertSupersedable(flightEventKind)
	delayAlert := flightEventKind == "delay_prediction"
	if stateAlert {
		var current feeds.Flight
		if err := json.Unmarshal(latestFlight, &current); err != nil {
			logger.Error("could not validate flight push state", "job", job.id, "error", err)
			return
		}
		now := time.Now()
		if flightEventKind == "boarding" {
			expires = min(expires, job.message.Timestamp.Add(boardingAlertLifetime).UnixNano())
		}
		if flightEventKind == "check_in" {
			departure := checkInDeadline(current, now)
			if departure == nil {
				s.removeObsoletePush(ctx, logger, job)
				return
			}
			expires = min(expires, departure.UnixNano())
		}
		if newerFlightEvent || !flightAlertRelevant(flightEventKind, current) || expires <= now.UnixNano() {
			s.removeObsoletePush(ctx, logger, job)
			return
		}
		if delayAlert {
			var forecast *Prediction
			if err := json.Unmarshal(latestForecast, &forecast); err != nil {
				logger.Error("could not validate delay push forecast", "job", job.id, "error", err)
				return
			}
			if forecast == nil || forecast.DelayMinutes < flightDelayAlertMinutes {
				s.removeObsoletePush(ctx, logger, job)
				return
			}
			var rotation []feeds.Flight
			if usesInboundTiming(forecast) {
				if err := json.Unmarshal(latestRotation, &rotation); err != nil {
					logger.Error("could not validate delay push rotation", "job", job.id, "error", err)
					return
				}
			}
			validUntil, valid := predictionValidity(forecast, current, rotation, now)
			if !valid || !s.terminalWeatherForecastCurrent(forecast, current, now) {
				s.removeObsoletePush(ctx, logger, job)
				return
			}
			// The saved forecast can use different evidence from the queued
			// event. Its revisions and the original job's evidence both apply.
			revised, err := forecastAdvisoryRevised(ctx, s.db, forecast, now)
			if err != nil {
				if ctx.Err() == nil {
					logger.Error("could not validate delay push evidence", "job", job.id, "error", err)
				}
				return
			}
			if revised {
				s.removeObsoletePush(ctx, logger, job)
				return
			}
			if validUntil != nil {
				expires = min(expires, validUntil.UnixNano())
			}
			job.message.Body = truncate(delayForecastAlertBody(current.Number, forecast.DelayMinutes), 180)
		}
	}
	apnsFamily := stateAlert && job.target.Platform == "ios"
	if apnsFamily {
		// Keep related APNs requests in submission order. A previous send can
		// still be in flight after its superseded SQLite row was removed.
		job.message.APNsCollapseID = hash("apns-flight-alert\x00" + job.owner + "\x00" + job.message.FlightID + "\x00" + flightEventKind)
		key := hash(strings.ToLower(job.target.Token) + "\x00" + job.message.APNsCollapseID)
		if _, sending := s.apnsFlightAlerts.LoadOrStore(key, struct{}{}); sending {
			s.deferFlightAlert(ctx, logger, job)
			return
		}
		defer s.apnsFlightAlerts.Delete(key)
	}
	window := job.message.AdvisoryWindow
	if window != nil && !hasAdvisoryEvidence {
		s.removeObsoletePush(ctx, logger, job)
		return
	}
	if hasAdvisoryEvidence {
		revised, err := outboxAdvisoryRevised(ctx, s.db, job.id, time.Now(), window != nil)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("could not validate advisory push evidence", "job", job.id, "error", err)
			}
			return
		}
		if revised {
			s.removeObsoletePush(ctx, logger, job)
			return
		}
	}
	if hasAdvisoryEvidence || apnsFamily || delayAlert {
		// A revision or revocation can retire the job while evidence is read
		// or while a previous same-family send releases its gate.
		var leaseExpiry int64
		var advisoryFlight []byte
		var sameDelayInputs bool
		if err := s.db.QueryRowContext(ctx, `SELECT o.expires,
CASE WHEN ? THEN (SELECT snapshot FROM flights WHERE id=o.flight_id) END,
CASE WHEN ? THEN EXISTS(SELECT 1 FROM flights f WHERE f.id=o.flight_id
  AND f.snapshot=? AND f.prediction=? AND f.inbound=?) ELSE 1 END`+leasedRow,
			window != nil, delayAlert, string(latestFlight), string(latestForecast), string(latestRotation),
			job.id, job.deviceID, job.lease).Scan(&leaseExpiry, &advisoryFlight, &sameDelayInputs); err != nil {
			if !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
				logger.Error("could not revalidate push lease", "job", job.id, "error", err)
			}
			return
		}
		if !sameDelayInputs {
			s.deferFlightAlert(ctx, logger, job)
			return
		}
		expires = min(expires, leaseExpiry)
		if window != nil {
			// Advisory collapse IDs group notice revisions and do not join
			// to the timeline event that supplied the original operation.
			var current feeds.Flight
			if err := json.Unmarshal(advisoryFlight, &current); err != nil {
				logger.Error("could not validate advisory push operation", "job", job.id, "error", err)
				return
			}
			advisory := feeds.Advisory{Kind: window.Kind, Airport: window.Airport, Direction: window.Direction, AirportContextOnly: window.Kind != ""}
			if advisory.Kind == "" {
				// Earlier outbox windows described recurring airport closures.
				advisory.Kind = "airport_closure"
			}
			now := time.Now()
			target, _, ok := advisoryAlertOperation(current, advisory, now)
			if !ok || target.Before(window.StartsAt) || !target.Before(window.EndsAt) {
				s.removeObsoletePush(ctx, logger, job)
				return
			}
			if window.Kind == "ground_stop" || window.Kind == "ground_delay" {
				cancelled, err := tfmsOutboxOperationCancelled(ctx, s.db, job.id, target, now)
				if err != nil {
					if ctx.Err() == nil {
						logger.Error("could not validate TFMS cancellation window", "job", job.id, "error", err)
					}
					return
				}
				if cancelled {
					s.removeObsoletePush(ctx, logger, job)
					return
				}
			}
			expires = min(expires, window.EndsAt.UnixNano())
		}
	}
	start := time.Now()
	deadline := time.Unix(0, min(job.lease, expires, start.Add(10*time.Second).UnixNano()))
	if ctx.Err() != nil || !deadline.After(start) {
		return
	}
	if expires < job.message.ExpiresAt.UnixNano() {
		job.message.ExpiresAt = time.Unix(0, expires).UTC()
	}
	if job.target.Platform == "android" && flightEventKind != "" {
		// The event join identifies ordinary flight alerts even when the job
		// was queued before Android supported alert ordering.
		job.message.AlertKind = flightEventKind
		job.message.AlertPhase = flightPhase
	}
	job.message.AdvisoryWindow = nil
	attempt, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if job.message.Kind == push.LiveStart {
		authorized, err := s.authorizeLiveStart(attempt, &job, start)
		if err != nil {
			if attempt.Err() == nil {
				logger.Error("could not authorize live start", "job", job.id, "error", err)
			}
			return
		}
		if !authorized {
			return
		}
	}
	if friendAlert {
		var revoked bool
		err := s.db.QueryRowContext(attempt, `SELECT `+revokedFriendAlert+leasedRow, job.id, job.deviceID, job.lease).Scan(&revoked)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) && attempt.Err() == nil {
				logger.Error("could not authorize friend push", "job", job.id, "error", err)
			}
			return
		}
		if revoked {
			s.removeObsoletePush(ctx, logger, job)
			return
		}
	}
	if job.message.ATFMRevision != "" {
		if job.target.Platform == "ios" {
			key := hash("atfm-slot\x00" + strings.ToLower(job.target.Token) + "\x00" + job.message.APNsCollapseID)
			if _, sending := s.apnsFlightAlerts.LoadOrStore(key, struct{}{}); sending {
				s.deferFlightAlert(ctx, logger, job)
				return
			}
			defer s.apnsFlightAlerts.Delete(key)
		}
		current, err := s.atfmAlertCurrent(attempt, job, time.Now())
		if err != nil {
			if attempt.Err() == nil {
				logger.Error("could not validate ATFM push revision", "job", job.id, "error", err)
			}
			return
		}
		if !current {
			s.removeObsoletePush(ctx, logger, job)
			return
		}
		job.message.ATFMRevision = ""
	}
	if job.message.InboundLegID != "" {
		current, err := s.inboundMilestoneCurrent(attempt, job, flightEventKind, time.Now())
		if err != nil {
			if attempt.Err() == nil {
				logger.Error("could not validate inbound alert scope", "job", job.id, "error", err)
			}
			return
		}
		if !current {
			s.removeObsoletePush(ctx, logger, job)
			return
		}
		job.message.InboundLegID = ""
	}
	if flightEventKind == "check_in" {
		authorized, err := s.authorizeCheckIn(attempt, job, time.Now())
		if err != nil {
			if attempt.Err() == nil {
				logger.Error("could not authorize check-in push", "job", job.id, "error", err)
			}
			return
		}
		if !authorized {
			return
		}
	}
	err = sender.Send(attempt, job.target, job.message)
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		// Shutdown interrupted transport without a provider result. Keep the
		// existing lease so a later dispatcher can recover the job.
		return
	}
	if err != nil {
		logger.Warn("push delivery failed", "job", job.id, "platform", job.target.Platform)
	} else {
		logger.Debug("push accepted by provider", "job", job.id, "platform", job.target.Platform, "request_ms", time.Since(start).Milliseconds(), "observation_to_send_ms", start.Sub(job.message.Timestamp).Milliseconds())
	}
	// The dispatcher waits for its workers before closing SQLite. Persist a
	// returned transport result even if the shutdown signal just arrived.
	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelFinish()
	if err := s.resolve(finish, job, err, time.Now()); err != nil {
		logger.Error("could not resolve push job", "job", job.id, "error", err)
	}
}

func (s *Store) deferFlightAlert(ctx context.Context, logger *slog.Logger, job deliveryJob) {
	due := time.Now().Add(dispatchPollInterval).UnixNano()
	if _, err := s.db.ExecContext(ctx, `UPDATE outbox SET lease_until=0,due=MAX(due,?)
WHERE id=? AND device_id=? AND lease_until=? AND done=0`, due, job.id, job.deviceID, job.lease); err != nil && ctx.Err() == nil {
		logger.Error("could not defer flight alert", "job", job.id, "error", err)
	}
}

func (s *Store) removeObsoletePush(ctx context.Context, logger *slog.Logger, job deliveryJob) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM outbox WHERE id=? AND device_id=? AND lease_until=? AND done=0`,
		job.id, job.deviceID, job.lease); err != nil && ctx.Err() == nil {
		logger.Error("could not remove obsolete push", "job", job.id, "error", err)
	}
}

func (s *Store) Prune(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM live_started WHERE (device_id,flight_id) IN (
SELECT o.device_id,o.flight_id FROM outbox o JOIN devices d ON d.id=o.device_id
LEFT JOIN outbox_live_start_leases l ON l.outbox_id=o.id
WHERE o.lease_until<=? AND o.expires<=? AND o.done=0 AND json_extract(o.message,'$.Kind')=?
AND (d.platform<>'ios' OR `+possibleLiveStart+`=0))`, now.UnixNano(), now.UnixNano(), push.LiveStart); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE lease_until<=? AND (expires<=? OR (done=1 AND due<=?))`, now.UnixNano(), now.UnixNano(), now.Add(-24*time.Hour).UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM shares WHERE expires<=?`, now.UnixNano()); err != nil {
		return err
	}
	// A withdrawn or shortened TFMS revision must outlive every older packet
	// still eligible for admission, even when the revision itself has expired.
	tfmsCutoff := now.Add(-tfmsAdvisoryRetention).UnixNano()
	if _, err := tx.ExecContext(ctx, `DELETE FROM pushed_advisories WHERE expires_ns<=? AND observed_ns<=?
AND (observed_ns<=? OR NOT `+tfmsAdvisoryFamilySQL+`)`, now.UnixNano(), now.Add(-signedAdvisoryRetention).UnixNano(), tfmsCutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pushed_advisory_predecessors WHERE EXISTS (
SELECT 1 FROM pushed_advisories a WHERE a.source=pushed_advisory_predecessors.source AND a.id=pushed_advisory_predecessors.id
AND a.observed_ns<=?)`, now.UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM advisory_supersessions AS r WHERE (observed_ns<? AND
(observed_ns<? OR NOT (source='faa_swim' AND (predecessor_id GLOB 'tfms/gs/*' OR predecessor_id GLOB 'tfms/gdp/*')))) OR
EXISTS (SELECT 1 FROM advisory_supersessions newer WHERE newer.source=r.source AND newer.airport=r.airport
AND newer.predecessor_id=r.predecessor_id AND newer.successor_id=r.successor_id
AND newer.observed_ns>r.observed_ns AND newer.observed_ns<=?)`,
		now.Add(-advisorySupersessionRetention).UnixNano(), tfmsCutoff, now.UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tfms_program_cancellations WHERE ends_ns<=? AND observed_ns<=?`,
		now.UnixNano(), now.Add(-signedAdvisoryRetention).UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM live_wire_clocks WHERE wire_epoch<=?`, now.Add(-7*24*time.Hour).Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM radar_positions WHERE at_ns<=?`, now.Add(-7*24*time.Hour).UnixNano()); err != nil {
		return err
	}
	if err := pruneTrafficRadarPositions(ctx, tx, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rotation_radar_positions WHERE at_ns<=?`, now.Add(-7*24*time.Hour).UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM friend_invites WHERE expires_ns<=?`, now.UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_import_receipts WHERE expires<=?`, now.Unix()); err != nil {
		return err
	}
	cutoff := now.Add(-atfmSlotRetention).UnixNano()
	if _, err := tx.ExecContext(ctx, `DELETE FROM atfm_slots WHERE observed_ns<=?`, cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM atfm_scope_floors WHERE changed_ns<=?`, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveDevice(ctx context.Context, owner string, d Device) error {
	if d.LiveSlot != "" || d.LiveRevision != 0 {
		return ErrInvalidLiveSlot
	}
	// Bound iOS slots use revision-based revocation so a delayed exact-token
	// deletion cannot remove a newer session that reused the token.
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE owner=? AND platform=? AND kind=? AND flight_id=?
AND (token=? OR (?='' AND ?<>''))
AND (?='' OR EXISTS(SELECT 1 FROM device_bindings WHERE device_id=devices.id AND binding=?))
AND (platform<>'ios' OR NOT EXISTS(SELECT 1 FROM device_bindings WHERE device_id=devices.id))`,
		owner, d.Platform, d.Kind, d.FlightID, d.Token, d.Token, d.Binding, d.Binding, d.Binding)
	return err
}

// RemoveIOSDevicesThroughRevision fences requests issued before the iOS
// installation opted out, including requests the server has yet to receive.
func (s *Store) RemoveIOSDevicesThroughRevision(ctx context.Context, owner string, revision int64) error {
	if revision <= 0 {
		return ErrInvalidLiveSlot
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO ios_device_floors(owner,revision) VALUES(?,?)
ON CONFLICT(owner) DO UPDATE SET revision=MAX(ios_device_floors.revision,excluded.revision)`, owner, revision); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE owner=? AND platform='ios'
AND COALESCE((SELECT revision FROM ios_alert_revisions WHERE device_id=devices.id),
             (SELECT revision FROM device_live_slots WHERE device_id=devices.id),0)
 <= (SELECT revision FROM ios_device_floors WHERE owner=?)`, owner, owner); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveDevices(ctx context.Context, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE owner=?`, owner)
	return err
}
