package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

const (
	signedAdvisoryRetention = feeds.AdvisoryMaxAge + time.Hour
	tfmsAdvisoryRetention   = feeds.TFMSAdvisoryMaxAge + time.Hour
)

const advisoryAlertSchema = `
CREATE TABLE IF NOT EXISTS advisory_alert_heads (
  flight_id TEXT NOT NULL REFERENCES flights(id) ON DELETE CASCADE,
  scope TEXT NOT NULL, state_id TEXT NOT NULL, observed_ns INTEGER NOT NULL, applied_ns INTEGER NOT NULL,
  PRIMARY KEY(flight_id,scope)
);`

func advisoryEvent(flight feeds.Flight, a feeds.Advisory, at time.Time, cancellations ...tfmsProgramCancellation) (Event, bool) {
	if a.Kind == feeds.AdvisoryAirportNotice {
		return Event{}, false
	}
	// Keep the original payload for dispatch-time revision checks, even when
	// an older record needs its fixed NOTAM times supplied at this boundary.
	original := a
	decoded, recognized := feeds.DecodeNOTAM(a.RawReason)
	var decodedAirport string
	if recognized {
		if decoded.Scope == "cancellation" {
			return Event{}, false
		}
		// Older snapshots classified daily closures as restrictions. Leave
		// them as context until a source supplies a full closure revision.
		if a.Kind == "airport_restriction" && decoded.HasDailyClosure() {
			return Event{}, false
		}
		if a.Kind == "airport_closure" && decoded.HasValidity {
			if !decoded.FullAirportClosure || !decoded.MatchesValidity(a.StartsAt, a.EndsAt) {
				return Event{}, false
			}
			a.StartsAt, a.EndsAt = decoded.StartsAt, decoded.EndsAt
			decodedAirport = decoded.Airport
		}
	}
	target, arrivalProgram, pending := advisoryAlertOperation(flight, a, at)
	if !pending {
		return Event{}, false
	}
	var cancellation time.Time
	if a.IsTFMSAirportProgram() {
		cancellation = latestTFMSProgramCancellation(cancellations, a.ID, target)
		if cancellation.After(a.ObservedAt) {
			return Event{}, false
		}
	}
	code, icao := flight.Origin, flight.OriginICAO
	if arrivalProgram {
		code, icao = flight.Destination, flight.DestinationICAO
	}
	if decodedAirport != "" && decodedAirport != code && decodedAirport != icao {
		return Event{}, false
	}
	a, active := advisoryOccurrence(a, decoded, target, at)
	if !active || a.StartsAt != nil && a.StartsAt.After(target) || a.EndsAt != nil && !a.EndsAt.After(target) {
		return Event{}, false
	}
	validUntil := advisoryForecastValidity(a, at)
	if !validUntil.After(at) {
		return Event{}, false
	}
	body := a.Reason
	family := hash(flight.ID + "\x00advisory\x00" + a.Source + "\x00" + a.ID)
	scope := family
	evidence := advisoryEvidence(original)
	revision := family + "\x00" + evidence.StateHash
	if !cancellation.IsZero() && cancellation.Before(a.ObservedAt) {
		// A newer definition can restore the same state after a cancellation.
		revision += "\x00tfms-restored\x00" + cancellation.UTC().Format(time.RFC3339Nano)
	}
	var window *push.AdvisoryWindow
	if a.Kind == "airport_closure" && decoded.HasDailyClosure() {
		body = "Published closure window ends " + a.EndsAt.UTC().Format("Jan 2 at 15:04 UTC") + "."
		direction := "departure"
		if arrivalProgram {
			direction = "arrival"
		}
		window = &push.AdvisoryWindow{Airport: a.Airport, Direction: direction, StartsAt: *a.StartsAt, EndsAt: *a.EndsAt}
		occurrence := "\x00" + direction + "\x00" + a.StartsAt.UTC().Format(time.RFC3339)
		revision += occurrence
		scope = hash(family + occurrence)
	} else if a.AirportContextOnly && (a.Kind == "ground_stop" || a.Kind == "ground_delay") && a.StartsAt != nil && a.EndsAt != nil {
		window = &push.AdvisoryWindow{Airport: a.Airport, Direction: a.Direction, StartsAt: *a.StartsAt, EndsAt: *a.EndsAt, Kind: a.Kind}
	}
	if body == "" {
		body = flight.Number
	}
	return Event{ID: hash(revision), Kind: a.Kind,
		Title: fmt.Sprintf("%s · %s", a.Airport, strings.ReplaceAll(a.Kind, "_", " ")), Body: body, At: at,
		validUntil: &validUntil, collapseID: family, advisoryScope: scope,
		advisoryEvidence: []forecastAdvisory{evidence}, advisoryWindow: window}, true
}

// saveAdvisoryEvent deduplicates consecutive states within each affected
// occurrence, while allowing a later revision to restore an earlier state.
func saveAdvisoryEvent(ctx context.Context, tx *sql.Tx, flightID string, event Event) error {
	evidence := event.advisoryEvidence[0]
	state, observed, applied := event.ID, evidence.ObservedNS, event.At.UnixNano()
	var priorState string
	var priorObserved, priorApplied int64
	err := tx.QueryRowContext(ctx, `SELECT state_id,observed_ns,applied_ns FROM advisory_alert_heads WHERE flight_id=? AND scope=?`,
		flightID, event.advisoryScope).Scan(&priorState, &priorObserved, &priorApplied)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if priorState != "" && (observed < priorObserved || observed == priorObserved && applied <= priorApplied) {
		return nil
	}
	changed := state != priorState
	var reopened bool
	if (priorState == "" || !changed) && observed >= priorObserved {
		reopened, err = advisoryReopened(ctx, tx, evidence, priorObserved, priorApplied, applied)
		if err != nil {
			return err
		}
	}
	changed = changed || reopened
	if changed {
		// A signed callback can arrive behind a newer poll whose flight
		// refresh has not run yet. Check the same source order as dispatch.
		payload, err := json.Marshal(event.advisoryEvidence)
		if err != nil {
			return err
		}
		revised, err := advisoryEvidenceRevised(ctx, tx, jsonAdvisoryEvidenceSQL, string(payload), event.At, false, true)
		if err != nil || revised {
			return err
		}
		if priorState != "" || reopened {
			event.ID = hash(state + "\x00" + strconv.FormatInt(observed, 10) + "\x00" + strconv.FormatInt(applied, 10))
			if err := retirePriorAdvisoryAlerts(ctx, tx, flightID, event); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO advisory_alert_heads(flight_id,scope,state_id,observed_ns,applied_ns) VALUES(?,?,?,?,?)
ON CONFLICT(flight_id,scope) DO UPDATE SET state_id=excluded.state_id,observed_ns=excluded.observed_ns,applied_ns=excluded.applied_ns`,
		flightID, event.advisoryScope, state, observed, applied); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	// Without a recorded withdrawal, keep the original event ID on first
	// sight so existing databases can seed the head without a duplicate.
	return saveEvent(ctx, tx, flightID, event, true)
}

func retirePriorAdvisoryAlerts(ctx context.Context, tx *sql.Tx, flightID string, event Event) error {
	var occurrence any
	var direction string
	if window := event.advisoryWindow; window != nil && window.Kind == "" {
		occurrence, direction = window.StartsAt.UTC().Format(time.RFC3339Nano), window.Direction
	}
	// A return to the old content would otherwise reauthorize jobs from its
	// first appearance. Leave independent closure occurrences alone.
	_, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE flight_id=?1 AND done=0
AND json_extract(message,'$.Kind')='alert' AND json_extract(message,'$.CollapseID')=?2
AND CASE WHEN ?3 IS NULL THEN json_extract(message,'$.AdvisoryWindow.StartsAt') IS NULL
  OR json_extract(message,'$.AdvisoryWindow.Kind')<>''
ELSE COALESCE(json_extract(message,'$.AdvisoryWindow.Kind'),'')=''
  AND json_extract(message,'$.AdvisoryWindow.Direction')=?4
  AND julianday(json_extract(message,'$.AdvisoryWindow.StartsAt'))=julianday(?3) END`,
		flightID, event.collapseID, occurrence, direction)
	return err
}

func advisoryAlertOperation(flight feeds.Flight, a feeds.Advisory, at time.Time) (time.Time, bool, bool) {
	target := advisoryTime(flight, a)
	destinationProgram := a.Direction != "departure" && (a.Airport == flight.Destination || a.Airport == flight.DestinationICAO)
	contextualStop := a.AirportContextOnly && a.Kind == "ground_stop" && destinationProgram
	if contextualStop {
		target = contextualGroundStopDeparture(flight)
	}
	if target == nil || target.Before(at.Add(-time.Hour)) {
		return time.Time{}, false, false
	}
	arrival := a.Kind != "ground_stop" && destinationProgram
	landingEndsOperation := a.Kind == "airport_closure" || a.AirportContextOnly && a.Kind == "ground_delay" && arrival
	operationEnded := landingEndsOperation && (flight.ActualLanding != nil || flight.ActualArrival != nil ||
		flight.Status == "landed" || flight.Status == "diverted")
	if operationEnded || arrival && (flight.ActualArrival != nil || finished(flight)) || !arrival && !contextualStop && !flightBeforeDeparture(flight) {
		return time.Time{}, false, false
	}
	if target.Before(at) {
		return at, arrival, true
	}
	return *target, arrival, true
}

// A ground stop can still apply after the aircraft pushes back and starts taxiing.
func contextualGroundStopDeparture(flight feeds.Flight) *time.Time {
	if flight.ActualTakeoff != nil || flight.ActualLanding != nil || flight.ActualArrival != nil || finished(flight) {
		return nil
	}
	switch flight.Status {
	case "airborne", "landed", "diverted":
		return nil
	case "taxi_out":
		if !flight.ObservedAt.IsZero() {
			return &flight.ObservedAt
		}
	}
	return effectiveDeparture(flight)
}

func (s *Store) ignoreLegacyTFMSReplay(a feeds.Advisory) bool {
	// Saved definitions are upgraded at startup. Older installations may have
	// already pruned a newer withdrawal for an incoming pre-cutoff definition.
	return s.tfmsLegacyReplayCutoff != 0 && a.ObservedAt.UnixNano() <= s.tfmsLegacyReplayCutoff && a.IsTFMSAirportProgram()
}

// PublishAdvisory commits the normalized program and affected flight alert
// jobs together. A repeated or earlier program observation cannot add work.
func (s *Store) PublishAdvisory(ctx context.Context, a feeds.Advisory, now time.Time) error {
	if s.ignoreLegacyTFMSReplay(a) {
		return nil
	}
	payload, err := json.Marshal(a)
	if err != nil {
		return err
	}
	expires := a.Expiry()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if a.Kind == feeds.AdvisoryTFMSCancellation {
		if !a.IsTFMSProgramCancellation() {
			return feeds.ErrInvalidQuery
		}
		if err := recordTFMSProgramCancellation(ctx, tx, a); err != nil {
			return err
		}
		return tx.Commit()
	}
	future := a.ObservedAt.After(now)
	if future {
		// Older revisions, including withdrawals, can still become effective
		// before the latest observation when provider clocks run ahead.
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pushed_advisory_predecessors(source,id,observed_ns,expires_ns,data)
SELECT source,id,observed_ns,expires_ns,data FROM pushed_advisories WHERE source=? AND id=? AND observed_ns<?`,
			a.Source, a.ID, a.ObservedAt.UnixNano()); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO pushed_advisories(source,id,observed_ns,expires_ns,data) VALUES(?,?,?,?,?)
ON CONFLICT(source,id) DO UPDATE SET observed_ns=excluded.observed_ns,expires_ns=excluded.expires_ns,data=excluded.data
WHERE excluded.observed_ns>pushed_advisories.observed_ns`, a.Source, a.ID, a.ObservedAt.UnixNano(), expires.UnixNano(), string(payload))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	var retained int64
	if n == 0 {
		// An observation can arrive after a newer but future-dated revision.
		// Retain it if it is the most recent eligible observation so far.
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pushed_advisory_predecessors(source,id,observed_ns,expires_ns,data)
SELECT ?,?,?,?,? WHERE EXISTS(SELECT 1 FROM pushed_advisories WHERE source=? AND id=? AND observed_ns>? AND observed_ns>?)
AND (? OR NOT EXISTS(SELECT 1 FROM pushed_advisory_predecessors WHERE source=? AND id=? AND observed_ns BETWEEN ? AND ?))`,
			a.Source, a.ID, a.ObservedAt.UnixNano(), expires.UnixNano(), string(payload), a.Source, a.ID, now.UnixNano(), a.ObservedAt.UnixNano(),
			future, a.Source, a.ID, a.ObservedAt.UnixNano(), now.UnixNano())
		if err != nil {
			return err
		}
		retained, err = result.RowsAffected()
		if err != nil {
			return err
		}
	}
	if future || retained != 0 {
		if err := boundPendingAdvisoryRevisions(ctx, tx, a.Source, a.ID, now); err != nil {
			return err
		}
	}
	if wholeAdvisoryWithdrawal(a) {
		if n+retained != 0 {
			if err := recordAdvisoryWithdrawal(ctx, tx, a, now, true); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if n+retained != 0 {
		// A signed active revision wins over a polled withdrawal at the
		// same source time, including before a future revision takes effect.
		if _, err := tx.ExecContext(ctx, `UPDATE advisory_withdrawals SET cleared_ns=?
WHERE source=? AND id=? AND observed_ns=? AND signed=0 AND cleared_ns=0`,
			now.UnixNano(), a.Source, a.ID, a.ObservedAt.UnixNano()); err != nil {
			return err
		}
	}
	if n+retained != 0 && a.SupersedesID != "" && a.Airport != "" && a.SupersedesID != a.ID &&
		!a.ObservedAt.Before(now.Add(-advisorySupersessionRetention)) && !a.ObservedAt.After(now.Add(advisorySupersessionLead)) {
		if err := storeAdvisorySupersessions(ctx, tx, `SELECT ?,?,?,?,?`, a.Source, a.Airport,
			a.SupersedesID, a.ObservedAt.UnixNano(), a.ID); err != nil {
			return err
		}
	}
	if n+retained == 0 || future || !expires.After(now) || a.Airport == "" {
		return tx.Commit()
	}
	var superseded bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM advisory_supersessions
WHERE source=? AND airport=? AND predecessor_id=? AND observed_ns BETWEEN ? AND ?)`,
		a.Source, a.Airport, a.ID, a.ObservedAt.UnixNano(), now.UnixNano()).Scan(&superseded); err != nil {
		return err
	}
	if superseded {
		return tx.Commit()
	}
	var cancellations []tfmsProgramCancellation
	if a.IsTFMSAirportProgram() {
		cancellations, err = loadTFMSProgramCancellations(ctx, tx, a.Airport, "", now)
		if err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.snapshot FROM flights f WHERE EXISTS(SELECT 1 FROM tracked t WHERE t.flight_id=f.id)
AND (? IN (json_extract(f.snapshot,'$.origin'),json_extract(f.snapshot,'$.originICAO'),json_extract(f.snapshot,'$.destination'),json_extract(f.snapshot,'$.destinationICAO')))`, a.Airport)
	if err != nil {
		return err
	}
	defer rows.Close()
	var affected []struct {
		id    string
		event Event
	}
	for rows.Next() {
		var raw []byte
		var f feeds.Flight
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return err
		}
		if finished(f) {
			continue
		}
		if event, ok := advisoryEvent(f, a, now, cancellations...); ok {
			affected = append(affected, struct {
				id    string
				event Event
			}{f.ID, event})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range affected {
		if err := saveAdvisoryEvent(ctx, tx, item.id, item.event); err != nil {
			return err
		}
	}
	return s.commitOutbox(tx)
}

func boundPendingAdvisoryRevisions(ctx context.Context, tx *sql.Tx, source, id string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM pushed_advisory_predecessors WHERE source=? AND id=? AND observed_ns<(
SELECT MAX(observed_ns) FROM pushed_advisory_predecessors WHERE source=? AND id=? AND observed_ns<=?)`,
		source, id, source, id, now.UnixNano()); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pushed_advisory_predecessors WHERE source=? AND id=?`, source, id).Scan(&count); err != nil {
		return err
	}
	if count > 64 {
		return fmt.Errorf("%w: too many pending advisory revisions", feeds.ErrUnavailable)
	}
	return nil
}

type advisoryRevision struct {
	advisory  feeds.Advisory
	expiresNS int64
}

const currentPushedAdvisories = `SELECT source,id,observed_ns,expires_ns,data FROM pushed_advisories WHERE observed_ns<=?
UNION ALL
SELECT q.source,q.id,q.observed_ns,q.expires_ns,q.data FROM pushed_advisory_predecessors q JOIN pushed_advisories p USING(source,id)
WHERE p.observed_ns>? AND q.observed_ns=(SELECT MAX(observed_ns) FROM pushed_advisory_predecessors
WHERE source=q.source AND id=q.id AND observed_ns<=?)`

const retainedPushedAdvisories = ` FROM (` + currentPushedAdvisories + `)
WHERE (expires_ns>? OR observed_ns>? OR (observed_ns>? AND ` + tfmsAdvisoryFamilySQL + `))`

type advisoryQueryer interface {
	rowQueryer
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) pushedAdvisories(ctx context.Context, now time.Time) ([]advisoryRevision, error) {
	return readPushedAdvisories(ctx, s.db, now, "")
}

func retainedAdvisoryArgs(now time.Time) []any {
	t := now.UnixNano()
	return []any{t, t, t, t, now.Add(-signedAdvisoryRetention).UnixNano(), now.Add(-tfmsAdvisoryRetention).UnixNano()}
}

func readPushedAdvisories(ctx context.Context, db advisoryQueryer, now time.Time, filter string, args ...any) ([]advisoryRevision, error) {
	rows, err := db.QueryContext(ctx, `SELECT data,expires_ns`+retainedPushedAdvisories+filter+` ORDER BY source,id`,
		append(retainedAdvisoryArgs(now), args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var advisories []advisoryRevision
	for rows.Next() {
		var raw []byte
		var revision advisoryRevision
		if err := rows.Scan(&raw, &revision.expiresNS); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &revision.advisory); err != nil {
			return nil, err
		}
		advisories = append(advisories, revision)
	}
	return advisories, rows.Err()
}

func reconcilePolledFlightAdvisories(ctx context.Context, tx *sql.Tx, flight feeds.Flight, polled []feeds.Advisory, now time.Time) ([]feeds.Advisory, error) {
	onRoute := func(a feeds.Advisory) bool {
		return a.Airport != "" && (a.Airport == flight.Origin || a.Airport == flight.OriginICAO ||
			a.Airport == flight.Destination || a.Airport == flight.DestinationICAO)
	}
	var relevant []feeds.Advisory
	if polled != nil {
		relevant = make([]feeds.Advisory, 0)
	}
	var keys [][2]string
	for _, a := range polled {
		if onRoute(a) {
			relevant = append(relevant, a)
			keys = append(keys, [2]string{a.Source, a.ID})
		}
	}
	rawKeys, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	// A moved, expired or cancelled signed revision can still supersede a
	// relevant polled row, so include matching keys as well as route airports.
	pushed, err := readPushedAdvisories(ctx, tx, now, ` AND (
 (json_extract(data,'$.airport')<>'' AND json_extract(data,'$.airport') IN (?,?,?,?))
 OR (source,id) IN (SELECT json_extract(value,'$[0]'),json_extract(value,'$[1]') FROM json_each(?))
)`, flight.Origin, flight.OriginICAO, flight.Destination, flight.DestinationICAO, string(rawKeys))
	if err != nil {
		return nil, err
	}
	if len(pushed) > 0 && len(polled) > len(relevant) {
		// A newer poll can likewise move an older signed record off route.
		signedKeys := make(map[[2]string]bool, len(pushed))
		for _, revision := range pushed {
			a := revision.advisory
			signedKeys[[2]string{a.Source, a.ID}] = true
		}
		relevant = relevant[:0]
		for _, a := range polled {
			if onRoute(a) || signedKeys[[2]string{a.Source, a.ID}] {
				relevant = append(relevant, a)
			}
		}
	}
	merged := mergeAdvisories(relevant, pushed, now)
	merged = slices.DeleteFunc(merged, func(a feeds.Advisory) bool { return !onRoute(a) })
	if merged == nil {
		// The old all-airports read distinguished a feed outage from a
		// snapshot with no evidence relevant to this route.
		var available bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1`+retainedPushedAdvisories+`)`, retainedAdvisoryArgs(now)...).Scan(&available); err != nil {
			return nil, err
		}
		if available {
			merged = make([]feeds.Advisory, 0)
		}
	}
	return filterSupersededAdvisories(ctx, tx, merged, now)
}

func mergeAdvisories(polled []feeds.Advisory, pushed []advisoryRevision, now time.Time) []feeds.Advisory {
	future := func(a feeds.Advisory) bool { return a.ObservedAt.After(now) }
	if slices.ContainsFunc(polled, future) {
		polled = slices.DeleteFunc(slices.Clone(polled), future)
	}
	if len(pushed) == 0 {
		return polled
	}
	type key struct{ source, id string }
	latest := make(map[key]advisoryRevision, len(pushed))
	for _, r := range pushed {
		if !r.advisory.ObservedAt.After(now) {
			latest[key{r.advisory.Source, r.advisory.ID}] = r
		}
	}
	if len(latest) == 0 {
		return polled
	}
	merged := make([]feeds.Advisory, 0, len(polled)+len(pushed))
	seen := make(map[key]bool, len(pushed))
	for _, a := range polled {
		k := key{a.Source, a.ID}
		if r, ok := latest[k]; ok && !a.ObservedAt.After(r.advisory.ObservedAt) {
			continue
		}
		seen[k] = true
		merged = append(merged, a)
	}
	for _, r := range pushed {
		if !r.advisory.ObservedAt.After(now) && r.expiresNS > now.UnixNano() && !seen[key{r.advisory.Source, r.advisory.ID}] {
			merged = append(merged, r.advisory)
		}
	}
	return merged
}
