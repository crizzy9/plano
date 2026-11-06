package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func advisoryEvent(flight feeds.Flight, a feeds.Advisory, at time.Time) (Event, bool) {
	// Keep the original payload for dispatch-time revision checks, even when
	// an older record needs its fixed NOTAM times supplied at this boundary.
	original := a
	var decodedAirport string
	if decoded, ok := feeds.DecodeNOTAM(a.RawReason); ok {
		if decoded.Scope == "cancellation" {
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
	validUntil := advisoryForecastValidity(a, at)
	if !validUntil.After(at) {
		return Event{}, false
	}
	target := advisoryTime(flight, a)
	if target == nil || target.Before(at.Add(-time.Hour)) {
		return Event{}, false
	}
	arrivalProgram := a.Kind != "ground_stop" && a.Direction != "departure" &&
		(a.Airport == flight.Destination || a.Airport == flight.DestinationICAO)
	code, icao := flight.Origin, flight.OriginICAO
	if arrivalProgram {
		code, icao = flight.Destination, flight.DestinationICAO
	}
	if decodedAirport != "" && decodedAirport != code && decodedAirport != icao {
		return Event{}, false
	}
	if arrivalProgram && (flight.ActualArrival != nil || finished(flight)) || !arrivalProgram && !flightBeforeDeparture(flight) {
		return Event{}, false
	}
	if target.Before(at) {
		target = &at
	}
	if a.StartsAt != nil && a.StartsAt.After(*target) || a.EndsAt != nil && !a.EndsAt.After(*target) {
		return Event{}, false
	}
	body := a.Reason
	if body == "" {
		body = flight.Number
	}
	family := hash(flight.ID + "\x00advisory\x00" + a.Source + "\x00" + a.ID)
	evidence := advisoryEvidence(original)
	return Event{ID: hash(family + "\x00" + evidence.StateHash), Kind: a.Kind,
		Title: fmt.Sprintf("%s · %s", a.Airport, strings.ReplaceAll(a.Kind, "_", " ")), Body: body, At: at,
		validUntil: &validUntil, collapseID: family, advisoryEvidence: []forecastAdvisory{evidence}}, true
}

// PublishAdvisory commits the normalized program and affected flight alert
// jobs together. A repeated or earlier program observation cannot add work.
func (s *Store) PublishAdvisory(ctx context.Context, a feeds.Advisory, now time.Time) error {
	payload, err := json.Marshal(a)
	if err != nil {
		return err
	}
	expires := a.ObservedAt.Add(6 * time.Hour)
	if a.EndsAt != nil && a.EndsAt.Before(expires) {
		expires = *a.EndsAt
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
		if event, ok := advisoryEvent(f, a, now); ok {
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
		if err := saveEvent(ctx, tx, item.id, item.event, true); err != nil {
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

func (s *Store) pushedAdvisories(ctx context.Context, now time.Time) ([]advisoryRevision, error) {
	t := now.UnixNano()
	rows, err := s.db.QueryContext(ctx, `SELECT data,expires_ns FROM (`+currentPushedAdvisories+`)
WHERE expires_ns>? OR observed_ns>? ORDER BY source,id`, t, t, t, t, now.Add(-7*time.Hour).UnixNano())
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
