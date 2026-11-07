package core

import (
	"context"
	"database/sql"
	"encoding/hex"
	"strings"
)

const iosPushClockSchema = `
CREATE TABLE IF NOT EXISTS ios_push_token_clocks (
  token_hash TEXT NOT NULL, kind TEXT NOT NULL, clock TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK(revision>0), active INTEGER NOT NULL CHECK(active IN (0,1)),
  PRIMARY KEY(token_hash,kind,clock)
);
CREATE UNIQUE INDEX IF NOT EXISTS ios_push_token_active_clock ON ios_push_token_clocks(token_hash,kind) WHERE active=1;
CREATE INDEX IF NOT EXISTS ios_device_canonical_tokens ON devices(lower(token),kind) WHERE platform='ios';`

func iosPushTokenKey(token string) (string, bool) {
	if _, err := hex.DecodeString(token); err == nil {
		return strings.ToLower(token), true
	}
	return token, false
}

func iosPushClockStale(ctx context.Context, tx *sql.Tx, key string, d Device) (bool, error) {
	var stale bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ios_push_token_clocks
WHERE token_hash=?1 AND kind=?2 AND ((?3='' AND active=1) OR (clock=?3 AND (active=0 OR revision>=?4))))`,
		hash(key), d.Kind, d.PushClock, d.LiveRevision).Scan(&stale)
	return stale, err
}

func normalizeIOSPushToken(ctx context.Context, tx *sql.Tx, kind, key string) error {
	// Preserve the row and its activity state when migrating a case alias.
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE devices SET token=?1
WHERE platform='ios' AND kind=?2 AND token<>?1 AND lower(token)=?1`, key, kind); err != nil {
		return err
	}
	// A legacy database may contain both spellings of the same APNs token.
	_, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE platform='ios' AND kind=?2 AND token<>?1 AND lower(token)=?1`, key, kind)
	return err
}

func acceptIOSPushClock(ctx context.Context, tx *sql.Tx, key string, d Device) error {
	digest := hash(key)
	if _, err := tx.ExecContext(ctx, `UPDATE ios_push_token_clocks SET active=0
WHERE token_hash=? AND kind=? AND active=1 AND clock<>?`, digest, d.Kind, d.PushClock); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO ios_push_token_clocks(token_hash,kind,clock,revision,active) VALUES(?,?,?,?,1)
ON CONFLICT(token_hash,kind,clock) DO UPDATE SET revision=excluded.revision`, digest, d.Kind, d.PushClock, d.LiveRevision)
	return err
}
