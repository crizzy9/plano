package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
)

// Restore publishes a new private SQLite database with pickup and Friends
// access removed. The source snapshot is opened read-only.
func Restore(ctx context.Context, source, destination string) error {
	return writeSnapshot(ctx, source, destination, true)
}

func checkRestoreSidecars(destination string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(destination + suffix); err == nil {
			return fmt.Errorf("restore destination has a SQLite %s sidecar: %w", suffix, os.ErrExist)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func scrubRestoredSharing(ctx context.Context, path string) error {
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=rw&_foreign_keys=on&_journal_mode=DELETE&_secure_delete=on"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Older snapshots can predate Friends or its personal alert authorization.
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		tables[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if tables["outbox_friend_jobs"] {
		remove := `DELETE FROM outbox WHERE done=0
AND EXISTS (SELECT 1 FROM outbox_friend_jobs j WHERE j.outbox_id=outbox.id)`
		if tables["outbox_friend_personal"] {
			remove += ` AND NOT EXISTS (
  SELECT 1 FROM outbox_friend_personal p JOIN tracked t ON t.owner=p.owner AND t.flight_id=p.flight_id
  JOIN devices d ON d.id=outbox.device_id AND d.owner=p.owner
  WHERE p.outbox_id=outbox.id AND p.flight_id=outbox.flight_id
)`
		}
		if _, err := tx.ExecContext(ctx, remove); err != nil {
			return err
		}
	}

	// Removing the markers makes retained dual-authorized jobs ordinary personal
	// jobs. Remove child rows before their parent tables for older snapshots too.
	for _, table := range []string{
		"outbox_friend_grants", "outbox_friend_personal", "outbox_friend_jobs",
		"pickup_link_meta", "shares", "friend_alert_preferences", "friend_flights", "friend_relationships", "friend_invites",
	} {
		if tables[table] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
				return err
			}
		}
	}
	var invalidRelationships bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_foreign_key_check)`).Scan(&invalidRelationships); err != nil {
		return fmt.Errorf("check SQLite restore relationships: %w", err)
	}
	if invalidRelationships {
		return errors.New("SQLite restore failed its foreign key check")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.Close()
}
