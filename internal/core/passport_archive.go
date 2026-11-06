package core

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const passportArchiveSchema = `
CREATE TABLE IF NOT EXISTS passport_archive (
  owner TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  id TEXT NOT NULL, flight_date TEXT NOT NULL, snapshot TEXT NOT NULL,
  PRIMARY KEY(owner,id)
);
`

const maxPassportArchiveFlights = 10_000

var (
	ErrPassportArchiveFull = errors.New("passport archive flight limit reached")
	passportArchiveID      = regexp.MustCompile(`^archive-[a-f0-9]{64}$`)
)

// ImportPassportArchive stores past flights for one Passport without adding
// them to the shared flight cache or starting live tracking.
func (s *Store) ImportPassportArchive(ctx context.Context, owner string, flights []feeds.Flight) (added, duplicates int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM passport_archive WHERE owner=?`, owner).Scan(&existing); err != nil {
		return 0, 0, err
	}
	insert, err := tx.PrepareContext(ctx, `INSERT INTO passport_archive(owner,id,flight_date,snapshot)
VALUES(?,?,?,?) ON CONFLICT(owner,id) DO NOTHING`)
	if err != nil {
		return 0, 0, err
	}
	defer insert.Close()
	for _, flight := range flights {
		if _, err := time.Parse(time.DateOnly, flight.HistoryDate); err != nil || !passportArchiveID.MatchString(flight.ID) ||
			flight.Status != "arrived" || flight.Source != "flighty_csv" {
			return 0, 0, feeds.ErrInvalidQuery
		}
		snapshot, err := json.Marshal(flight)
		if err != nil {
			return 0, 0, err
		}
		result, err := insert.ExecContext(ctx, owner, flight.ID, flight.HistoryDate, string(snapshot))
		if err != nil {
			return 0, 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		if count == 0 {
			duplicates++
			continue
		}
		added++
		if existing+added > maxPassportArchiveFlights {
			return 0, 0, ErrPassportArchiveFull
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return added, duplicates, nil
}

func (s *Store) DeletePassportArchive(ctx context.Context, owner, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM passport_archive WHERE owner=? AND id=?`, owner, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return ErrNotFound
	}
	return err
}
