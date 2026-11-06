package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

// Retention follows the slot key, including withdrawals without a program window.
const tfmsAdvisoryFamilySQL = `(source='faa_swim' AND (id GLOB 'tfms/gs/*' OR id GLOB 'tfms/gdp/*'))`

const tfmsAdvisoryLifetimeSchema = `CREATE TABLE IF NOT EXISTS tfms_advisory_lifetime_policy (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1), legacy_replay_cutoff_ns INTEGER NOT NULL
)`

func upgradeTFMSAdvisoryDeadlines(db *sql.DB, now time.Time, legacy bool) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(tfmsAdvisoryLifetimeSchema); err != nil {
		return 0, err
	}
	var cutoff int64
	err = tx.QueryRow(`SELECT legacy_replay_cutoff_ns FROM tfms_advisory_lifetime_policy WHERE singleton=1`).Scan(&cutoff)
	if err == nil {
		return cutoff, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if legacy {
		// Earlier releases may have pruned a newer withdrawal in this range.
		cutoff = now.Add(-signedAdvisoryRetention).UnixNano()
		for _, table := range []string{"pushed_advisories", "pushed_advisory_predecessors"} {
			if err := upgradeTFMSAdvisoryTable(tx, table, now); err != nil {
				return 0, fmt.Errorf("extend stored TFMS advisory deadlines in %s: %w", table, err)
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO tfms_advisory_lifetime_policy(singleton,legacy_replay_cutoff_ns) VALUES(1,?)`, cutoff); err != nil {
		return 0, err
	}
	return cutoff, tx.Commit()
}

func upgradeTFMSAdvisoryTable(tx *sql.Tx, table string, now time.Time) error {
	const batchSize = 128
	legacyAge := feeds.AdvisoryMaxAge.Nanoseconds()
	cutoff := now.Add(-feeds.TFMSAdvisoryMaxAge).UnixNano()
	var lastID string
	var lastObserved int64
	type deadline struct {
		id                string
		observed, expires int64
	}
	for {
		// A deadline earlier than this cap already used the published end.
		rows, err := tx.Query(`SELECT id,observed_ns,data FROM `+table+`
WHERE `+tfmsAdvisoryFamilySQL+` AND expires_ns=observed_ns+? AND observed_ns>?
AND (id,observed_ns)>(?,?) ORDER BY id,observed_ns LIMIT ?`, legacyAge, cutoff, lastID, lastObserved, batchSize)
		if err != nil {
			return err
		}
		var updates []deadline
		count := 0
		for rows.Next() {
			count++
			var raw []byte
			if err := rows.Scan(&lastID, &lastObserved, &raw); err != nil {
				rows.Close()
				return err
			}
			var a feeds.Advisory
			if json.Unmarshal(raw, &a) != nil || a.ID != lastID || a.ObservedAt.UnixNano() != lastObserved || !a.IsTFMSAirportProgram() {
				continue
			}
			expires := a.Expiry()
			if expires.After(now) && expires.UnixNano() > lastObserved+legacyAge {
				updates = append(updates, deadline{lastID, lastObserved, expires.UnixNano()})
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, u := range updates {
			if _, err := tx.Exec(`UPDATE `+table+` SET expires_ns=? WHERE source=? AND id=? AND observed_ns=?`,
				u.expires, feeds.FAASWIM, u.id, u.observed); err != nil {
				return err
			}
		}
		if count < batchSize {
			return nil
		}
	}
}
