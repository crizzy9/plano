package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

func openReadOnly(path string) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_busy_timeout=5000"
	return sql.Open("sqlite3", dsn)
}

// Backup creates a private, standalone SQLite snapshot while Plano is running.
// It publishes the checked snapshot only if the destination does not exist.
func Backup(ctx context.Context, source, destination string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return err
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	switch destination {
	case source + "-wal", source + "-shm", source + "-journal":
		return errors.New("backup destination is reserved for a SQLite sidecar")
	}
	dir, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("backup destination: %w", os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	db, err := openReadOnly(source)
	if err != nil {
		return err
	}
	defer db.Close()
	staging, err := os.MkdirTemp(parent, ".plano-backup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	path := filepath.Join(staging, "snapshot.db")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("copy SQLite database: %w", err)
	}
	if err := checkBackup(ctx, path); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(path, destination); err != nil {
		return fmt.Errorf("publish backup: %w", err)
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("backup was published but directory sync failed: %w", err)
	}
	return nil
}

func checkBackup(ctx context.Context, path string) error {
	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&result); err != nil {
		return fmt.Errorf("check SQLite backup: %w", err)
	}
	if result != "ok" {
		return errors.New("SQLite backup failed its integrity check")
	}
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master
WHERE type='table' AND name IN ('installations','flights','tracked','outbox')`).Scan(&tables); err != nil {
		return err
	}
	if tables != 4 {
		return errors.New("backup source is missing Plano tables")
	}
	return nil
}
