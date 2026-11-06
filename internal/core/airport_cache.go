package core

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"
)

const airportCacheSchema = `
CREATE TABLE IF NOT EXISTS airport_coordinate_cache (
  code TEXT PRIMARY KEY,
  latitude REAL,
  longitude REAL,
  retry_after_ns INTEGER NOT NULL DEFAULT 0,
  CHECK ((latitude IS NULL AND longitude IS NULL) OR
         (latitude IS NOT NULL AND longitude IS NOT NULL AND
          latitude BETWEEN -90 AND 90 AND longitude BETWEEN -180 AND 180))
);
`

type AirportCoordinates struct {
	Latitude  float64
	Longitude float64
}

// AirportCacheEntry stores coordinates or a time to retry an unsuccessful lookup.
type AirportCacheEntry struct {
	Coordinates *AirportCoordinates
	RetryAfter  time.Time
}

func (s *Store) ReadAirportCoordinates(ctx context.Context, codes []string) (map[string]AirportCacheEntry, error) {
	cache := make(map[string]AirportCacheEntry)
	for batch := range slices.Chunk(codes, 200) {
		args := make([]any, len(batch))
		for i, code := range batch {
			args[i] = code
		}
		query := `SELECT code,latitude,longitude,retry_after_ns FROM airport_coordinate_cache
WHERE code IN (` + strings.Repeat("?,", len(batch)-1) + "?)"
		if err := s.readAirportCoordinateBatch(ctx, cache, query, args); err != nil {
			return nil, err
		}
	}
	return cache, nil
}

func (s *Store) readAirportCoordinateBatch(ctx context.Context, cache map[string]AirportCacheEntry, query string, args []any) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		var lat, lon sql.NullFloat64
		var retryAfter int64
		if err := rows.Scan(&code, &lat, &lon, &retryAfter); err != nil {
			return err
		}
		entry := AirportCacheEntry{}
		if lat.Valid && lon.Valid {
			entry.Coordinates = &AirportCoordinates{Latitude: lat.Float64, Longitude: lon.Float64}
		}
		if retryAfter != 0 {
			entry.RetryAfter = time.Unix(0, retryAfter)
		}
		cache[code] = entry
	}
	return rows.Err()
}

func (s *Store) SaveAirportCoordinates(ctx context.Context, entries map[string]AirportCacheEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO airport_coordinate_cache VALUES(?,?,?,?)
ON CONFLICT(code) DO UPDATE SET latitude=excluded.latitude, longitude=excluded.longitude, retry_after_ns=excluded.retry_after_ns
WHERE excluded.latitude IS NOT NULL OR
      (airport_coordinate_cache.latitude IS NULL AND excluded.retry_after_ns > airport_coordinate_cache.retry_after_ns)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for code, entry := range entries {
		var lat, lon any
		var retryAfter int64
		if entry.Coordinates != nil {
			lat, lon = entry.Coordinates.Latitude, entry.Coordinates.Longitude
		} else if !entry.RetryAfter.IsZero() {
			retryAfter = entry.RetryAfter.UnixNano()
		}
		if _, err := stmt.ExecContext(ctx, code, lat, lon, retryAfter); err != nil {
			return err
		}
	}
	return tx.Commit()
}
