package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const aeroSubscriptionSchema = `
CREATE TABLE IF NOT EXISTS managed_aero_subscriptions (
  scope_key TEXT PRIMARY KEY, scope TEXT NOT NULL, marker TEXT NOT NULL UNIQUE,
  target_hash TEXT NOT NULL, remote_id INTEGER UNIQUE, ends_ns INTEGER NOT NULL,
  inspect_after_ns INTEGER NOT NULL, rejected INTEGER NOT NULL DEFAULT 0 CHECK(rejected IN (0,1))
);
`

const (
	aeroSubscriptionMarker    = "plano_alert"
	aeroSubscriptionScanLimit = 512
	aeroSubscriptionRequests  = 8
	aeroSubscriptionCreates   = 2
	aeroSubscriptionPages     = 3
	aeroSubscriptionRetry     = 15 * time.Minute
)

type AeroSubscriptionProvider interface {
	AeroAlertEndpointConfigured(context.Context) (bool, error)
	ListAeroSubscriptions(context.Context, string) ([]feeds.AeroSubscription, string, error)
	InspectAeroSubscription(context.Context, int64) (feeds.AeroSubscription, error)
	CreateAeroSubscription(context.Context, feeds.AeroAlertScope, string) (int64, error)
	DeleteAeroSubscription(context.Context, int64) error
}

type AeroSubscriptionOptions struct {
	CallbackURL string
	MaxActive   int
	Interval    time.Duration
}

func (o AeroSubscriptionOptions) Validate() error {
	if o.MaxActive < 1 || o.MaxActive > 50 || o.Interval < time.Minute || o.Interval > time.Hour {
		return errors.New("AeroAPI subscription limits require 1 to 50 active scopes and an interval between 1m and 1h")
	}
	target, err := parseAeroCallbackURL(o.CallbackURL)
	if err != nil || !strings.HasSuffix(target.Path, "/v1/webhooks/aeroapi") || target.Query().Has(aeroSubscriptionMarker) {
		return errors.New("AeroAPI managed alerts require a valid HTTPS Plano callback URL")
	}
	if target.User == nil && len(target.Query().Get("token")) >= 32 && len(target.Query()["token"]) == 1 {
		return nil
	}
	if target.User != nil && target.User.Username() == "plano" && !target.Query().Has("token") {
		password, present := target.User.Password()
		if present && len(password) >= 32 {
			return nil
		}
	}
	return errors.New("AeroAPI managed alerts require a callback token or HTTPS Plano Basic authentication")
}

func parseAeroCallbackURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, errors.New("invalid AeroAPI callback URL")
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return nil, errors.New("invalid AeroAPI callback query")
	}
	parsed.RawQuery = query.Encode()
	return parsed, nil
}

func aeroCallbackTarget(base, marker string) string {
	target, err := parseAeroCallbackURL(base)
	if err != nil {
		return ""
	}
	query := target.Query()
	query.Set(aeroSubscriptionMarker, marker)
	target.RawQuery = query.Encode()
	return target.String()
}

func aeroTargetHash(raw string) string {
	target, err := parseAeroCallbackURL(raw)
	if err != nil {
		return ""
	}
	return hash(target.String())
}

type wantedAeroSubscription struct {
	key    string
	scope  feeds.AeroAlertScope
	starts time.Time
	ends   time.Time
}

type managedAeroSubscription struct {
	key, marker, targetHash string
	scope                   feeds.AeroAlertScope
	remoteID                sql.NullInt64
	rejected                bool
	ends, inspectAfter      time.Time
}

func (s *Store) wantedAeroSubscriptions(ctx context.Context, now time.Time) ([]wantedAeroSubscription, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT snapshot FROM flights WHERE EXISTS(SELECT 1 FROM tracked WHERE tracked.flight_id=flights.id)
AND json_extract(snapshot,'$.source') IN ('flightaware_aeroapi','flightaware_schedule')
AND julianday(json_extract(snapshot,'$.scheduledDeparture')) BETWEEN julianday(?) AND julianday(?)
ORDER BY julianday(json_extract(snapshot,'$.scheduledDeparture')),id LIMIT ?`,
		now.Add(-48*time.Hour).Format(time.RFC3339), now.Add(48*time.Hour).Format(time.RFC3339), aeroSubscriptionScanLimit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	wanted := make([]wantedAeroSubscription, 0)
	seen := make(map[string]bool)
	count := 0
	for rows.Next() {
		count++
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, false, err
		}
		var flight feeds.Flight
		if err := json.Unmarshal(raw, &flight); err != nil {
			return nil, false, err
		}
		scope, starts, ok := flight.AeroAlertScope()
		if !ok || finished(flight) {
			continue
		}
		// Local cleanup allows one extra calendar day for overnight or late
		// arrivals. AeroAPI's registration itself never widens its dates.
		ends := starts.AddDate(0, 0, 2)
		key := hash(strings.Join([]string{scope.Ident, scope.Origin, scope.Destination, scope.Date}, "\x00"))
		if !ends.After(now) || seen[key] {
			continue
		}
		wanted = append(wanted, wantedAeroSubscription{key: key, scope: scope, starts: starts, ends: ends})
		seen[key] = true
	}
	return wanted, count <= aeroSubscriptionScanLimit, rows.Err()
}

func (s *Store) managedAeroSubscriptions(ctx context.Context) ([]managedAeroSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope_key,scope,marker,target_hash,remote_id,ends_ns,inspect_after_ns,rejected
FROM managed_aero_subscriptions ORDER BY inspect_after_ns,scope_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var managed []managedAeroSubscription
	for rows.Next() {
		var row managedAeroSubscription
		var scope []byte
		var end, inspect int64
		if err := rows.Scan(&row.key, &scope, &row.marker, &row.targetHash, &row.remoteID, &end, &inspect, &row.rejected); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(scope, &row.scope); err != nil {
			return nil, err
		}
		row.ends, row.inspectAfter = time.Unix(0, end), time.Unix(0, inspect)
		managed = append(managed, row)
	}
	return managed, rows.Err()
}

// reserveAeroSubscription persists the single allowed create attempt before
// touching the provider. An interrupted or ambiguous POST is never retried.
func (s *Store) reserveAeroSubscription(ctx context.Context, row wantedAeroSubscription, opts AeroSubscriptionOptions, now time.Time) (managedAeroSubscription, string, bool, error) {
	marker := randomToken()
	target := aeroCallbackTarget(opts.CallbackURL, marker)
	rowState := managedAeroSubscription{key: row.key, marker: marker, targetHash: aeroTargetHash(target), scope: row.scope, ends: row.ends, inspectAfter: now.Add(aeroSubscriptionRetry)}
	if rowState.targetHash == "" {
		return rowState, "", false, errors.New("invalid AeroAPI callback target")
	}
	scope, err := json.Marshal(row.scope)
	if err != nil {
		return rowState, "", false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO managed_aero_subscriptions(scope_key,scope,marker,target_hash,ends_ns,inspect_after_ns)
SELECT ?,?,?,?,?,? WHERE (SELECT COUNT(*) FROM managed_aero_subscriptions WHERE rejected=0)<?
ON CONFLICT(scope_key) DO NOTHING`, rowState.key, scope, marker, rowState.targetHash, rowState.ends.UnixNano(), rowState.inspectAfter.UnixNano(), opts.MaxActive)
	if err != nil {
		return rowState, "", false, err
	}
	count, err := result.RowsAffected()
	return rowState, target, count == 1, err
}

func (s *Store) recordAeroRemote(ctx context.Context, row managedAeroSubscription, id int64, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE managed_aero_subscriptions SET remote_id=?,inspect_after_ns=?
WHERE scope_key=? AND marker=? AND (remote_id IS NULL OR remote_id=?)`, id, next.UnixNano(), row.key, row.marker, id)
	return err
}

func (s *Store) recordAeroRejection(ctx context.Context, row managedAeroSubscription) error {
	_, err := s.db.ExecContext(ctx, `UPDATE managed_aero_subscriptions SET rejected=1 WHERE scope_key=? AND marker=? AND remote_id IS NULL`, row.key, row.marker)
	return err
}

func (s *Store) deferAeroInspection(ctx context.Context, row managedAeroSubscription, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE managed_aero_subscriptions SET inspect_after_ns=? WHERE scope_key=? AND marker=?`, next.UnixNano(), row.key, row.marker)
	return err
}

func (s *Store) forgetAeroSubscription(ctx context.Context, row managedAeroSubscription) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM managed_aero_subscriptions WHERE scope_key=? AND marker=?`, row.key, row.marker)
	return err
}

func ownsAeroSubscription(row managedAeroSubscription, remote feeds.AeroSubscription) bool {
	return remote.ID > 0 && remote.MatchesScope(row.scope) && aeroTargetHash(remote.TargetURL) == row.targetHash
}

// RunAeroSubscriptions is an optional reconciler for one self-hosted store.
// Polling and independently provisioned callbacks remain usable without it.
func (s *Store) RunAeroSubscriptions(ctx context.Context, client AeroSubscriptionProvider, opts AeroSubscriptionOptions, logger *slog.Logger) {
	if err := opts.Validate(); err != nil {
		logger.Error("invalid AeroAPI subscription configuration", "error", err)
		return
	}
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := s.reconcileAeroSubscriptions(ctx, client, opts, time.Now(), logger); err != nil && ctx.Err() == nil {
			// Provider errors and callback URLs can contain credentials. The
			// reconciler only reports a generic cycle failure here.
			logger.Warn("AeroAPI alert subscription reconciliation unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Store) reconcileAeroSubscriptions(ctx context.Context, client AeroSubscriptionProvider, opts AeroSubscriptionOptions, now time.Time, logger *slog.Logger) error {
	wanted, complete, err := s.wantedAeroSubscriptions(ctx, now)
	if err != nil {
		return err
	}
	managed, err := s.managedAeroSubscriptions(ctx)
	if err != nil {
		return err
	}
	wantedKeys := make(map[string]bool, len(wanted))
	for _, row := range wanted {
		wantedKeys[row.key] = true
	}
	managedKeys := make(map[string]bool, len(managed))
	requests := aeroSubscriptionRequests
	active := 0
	var pending []managedAeroSubscription
	for _, row := range managed {
		if !row.ends.Add(24 * time.Hour).After(now) {
			// The provider scope already ended. The retention window allows
			// cleanup without keeping stale unresolved rows forever.
			if err := s.forgetAeroSubscription(ctx, row); err != nil {
				return err
			}
			continue
		}
		managedKeys[row.key] = true
		if row.rejected {
			continue
		}
		active++
		if !row.remoteID.Valid && !row.inspectAfter.After(now) {
			pending = append(pending, row)
			continue
		}
		if !row.remoteID.Valid || requests < 2 {
			continue
		}
		targetChanged := aeroTargetHash(aeroCallbackTarget(opts.CallbackURL, row.marker)) != row.targetHash
		retire := !row.ends.After(now) || complete && !wantedKeys[row.key] || targetChanged
		if !retire || row.inspectAfter.After(now) {
			continue
		}
		requests--
		remote, err := client.InspectAeroSubscription(ctx, row.remoteID.Int64)
		switch {
		case errors.Is(err, feeds.ErrNotFound):
		case err != nil:
			if err := s.deferAeroInspection(ctx, row, now.Add(aeroSubscriptionRetry)); err != nil {
				return err
			}
			logger.Warn("AeroAPI alert inspection unavailable", "alert_id", row.remoteID.Int64)
			continue
		case remote.ID != row.remoteID.Int64 || !ownsAeroSubscription(row, remote):
			logger.Warn("AeroAPI alert ownership changed; leaving remote alert untouched", "alert_id", row.remoteID.Int64)
			if err := s.deferAeroInspection(ctx, row, row.ends.Add(24*time.Hour)); err != nil {
				return err
			}
			continue
		default:
			requests--
			if err := client.DeleteAeroSubscription(ctx, remote.ID); err != nil && !errors.Is(err, feeds.ErrNotFound) {
				if err := s.deferAeroInspection(ctx, row, now.Add(aeroSubscriptionRetry)); err != nil {
					return err
				}
				logger.Warn("AeroAPI alert retirement unavailable", "alert_id", row.remoteID.Int64)
				continue
			}
		}
		if err := s.forgetAeroSubscription(ctx, row); err != nil {
			return err
		}
		delete(managedKeys, row.key)
		active--
	}
	if len(pending) > 0 && requests >= aeroSubscriptionPages {
		used, err := s.recoverAeroSubscriptions(ctx, client, pending, now)
		requests -= used
		if err != nil {
			return err
		}
	}
	creates := 0
	checkedEndpoint := false
	for _, row := range wanted {
		if active >= opts.MaxActive || requests < 1 || creates == aeroSubscriptionCreates {
			break
		}
		if managedKeys[row.key] || !row.starts.After(now) {
			continue
		}
		if !checkedEndpoint {
			if requests < 2 {
				break
			}
			requests--
			configured, err := client.AeroAlertEndpointConfigured(ctx)
			if err != nil {
				return err
			}
			if !configured {
				logger.Warn("AeroAPI alert setup requires an existing provider account endpoint")
				return nil
			}
			checkedEndpoint = true
		}
		state, target, reserved, err := s.reserveAeroSubscription(ctx, row, opts, now)
		if err != nil {
			return err
		}
		if !reserved {
			continue
		}
		requests--
		creates++
		active++
		managedKeys[row.key] = true
		id, err := client.CreateAeroSubscription(ctx, row.scope, target)
		if errors.Is(err, feeds.ErrInvalidQuery) {
			// A documented HTTP 400 rejection did not create an alert. Keep
			// the scope tombstone, without consuming an active quota slot.
			if err := s.recordAeroRejection(ctx, state); err != nil {
				return err
			}
			return errors.New("AeroAPI rejected an alert scope")
		}
		if err != nil || id <= 0 {
			// Stop spending requests this cycle. The next reconciliation can
			// recover an exact marker match but cannot POST this scope again.
			return errors.New("AeroAPI alert create has no confirmed remote identity")
		}
		if err := s.recordAeroRemote(ctx, state, id, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recoverAeroSubscriptions(ctx context.Context, client AeroSubscriptionProvider, pending []managedAeroSubscription, now time.Time) (int, error) {
	// An exact marker and scope must be unique across a completed bounded
	// inventory. No match, duplicate matches, or incomplete pages prove nothing.
	type match struct {
		id    int64
		count int
	}
	pendingByHash := make(map[string]managedAeroSubscription, len(pending))
	matches := make(map[string]match, len(pending))
	for _, row := range pending {
		pendingByHash[row.targetHash] = row
	}
	var cursor string
	calls := 0
	for calls < aeroSubscriptionPages {
		rows, next, err := client.ListAeroSubscriptions(ctx, cursor)
		calls++
		if err != nil {
			return calls, err
		}
		for _, remote := range rows {
			targetHash := aeroTargetHash(remote.TargetURL)
			if row, ok := pendingByHash[targetHash]; ok && remote.ID > 0 && remote.MatchesScope(row.scope) {
				found := matches[targetHash]
				found.id, found.count = remote.ID, found.count+1
				matches[targetHash] = found
			}
		}
		cursor = next
		if cursor == "" {
			break
		}
	}
	for _, row := range pending {
		if found := matches[row.targetHash]; cursor == "" && found.count == 1 {
			if err := s.recordAeroRemote(ctx, row, found.id, now); err != nil {
				return calls, err
			}
		} else if err := s.deferAeroInspection(ctx, row, now.Add(aeroSubscriptionRetry)); err != nil {
			return calls, err
		}
	}
	return calls, nil
}
