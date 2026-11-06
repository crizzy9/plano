package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func advisoryEvent(flight feeds.Flight, a feeds.Advisory, at time.Time) (Event, bool) {
	target := advisoryTime(flight, a)
	if target == nil || a.StartsAt != nil && a.StartsAt.After(*target) || a.EndsAt != nil && !a.EndsAt.After(*target) || target.Before(at.Add(-time.Hour)) {
		return Event{}, false
	}
	body := a.Reason
	if body == "" {
		body = flight.Number
	}
	return Event{ID: hash(flight.ID + "\x00advisory\x00" + a.Source + "\x00" + a.ID), Kind: a.Kind,
		Title: fmt.Sprintf("%s · %s", a.Airport, strings.ReplaceAll(a.Kind, "_", " ")), Body: body, At: at}, true
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
	if n == 0 || !expires.After(now) || a.Airport == "" {
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
		if err := saveEvent(ctx, tx, item.id, item.event); err != nil {
			return err
		}
	}
	return s.commitOutbox(tx)
}

type advisoryRevision struct {
	advisory  feeds.Advisory
	expiresNS int64
}

func (s *Store) pushedAdvisories(ctx context.Context, now time.Time) ([]advisoryRevision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data,expires_ns FROM pushed_advisories
WHERE expires_ns>? OR observed_ns>? ORDER BY source,id`, now.UnixNano(), now.Add(-7*time.Hour).UnixNano())
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
	if len(pushed) == 0 {
		return polled
	}
	type key struct{ source, id string }
	latest := make(map[key]advisoryRevision, len(pushed))
	for _, r := range pushed {
		latest[key{r.advisory.Source, r.advisory.ID}] = r
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
		if r.expiresNS > now.UnixNano() && !seen[key{r.advisory.Source, r.advisory.ID}] {
			merged = append(merged, r.advisory)
		}
	}
	return merged
}
