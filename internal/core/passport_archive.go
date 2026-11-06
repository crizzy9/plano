package core

import (
	"bytes"
	"context"
	"database/sql"
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
	ErrPassportArchiveFull     = errors.New("passport archive flight limit reached")
	ErrPassportArchiveConflict = errors.New("passport archive flight changed")
	passportArchiveID          = regexp.MustCompile(`^archive-[a-f0-9]{64}$`)
)

// ImportPassportArchive stores past flights for one Passport without adding
// them to the shared flight cache or starting live tracking.
func (s *Store) ImportPassportArchive(ctx context.Context, owner string, flights []feeds.Flight) (added, duplicates int, err error) {
	return s.insertPassportArchive(ctx, owner, flights, "flighty_csv")
}

// CreateManualPassportFlight inserts a private log entry. A repeated ID leaves
// the saved row intact, even if the installation has since edited it.
func (s *Store) CreateManualPassportFlight(ctx context.Context, owner string, flight feeds.Flight) (created bool, err error) {
	if flight.ObservedAt.IsZero() {
		return false, feeds.ErrInvalidQuery
	}
	flight.ObservedAt = flight.ObservedAt.UTC()
	added, _, err := s.insertPassportArchive(ctx, owner, []feeds.Flight{flight}, feeds.PassportManualSource)
	return added == 1, err
}

func (s *Store) insertPassportArchive(ctx context.Context, owner string, flights []feeds.Flight, source string) (added, duplicates int, err error) {
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
		if !validPassportArchiveFlight(flight, source) {
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
			// A reimport can fill timing data missing from the saved export.
			if source == "flighty_csv" && (flight.ScheduledDeparture != nil || flight.ScheduledArrival != nil || flight.ActualDeparture != nil || flight.ActualArrival != nil) {
				if err := fillPassportArchiveTimes(ctx, tx, owner, flight); err != nil {
					return 0, 0, err
				}
			}
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

func fillPassportArchiveTimes(ctx context.Context, tx *sql.Tx, owner string, incoming feeds.Flight) error {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT snapshot FROM passport_archive
WHERE owner=? AND id=? AND flight_date=? AND json_extract(snapshot,'$.source')='flighty_csv'`,
		owner, incoming.ID, incoming.HistoryDate).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved feeds.Flight
	if err := json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	if saved.Number != incoming.Number || saved.Origin != incoming.Origin || saved.Destination != incoming.Destination {
		return nil
	}
	patch := make(map[string]*time.Time, 4)
	for _, pair := range [...]struct {
		prefix                                       string
		departure, arrival, newDeparture, newArrival *time.Time
	}{
		{"scheduled", saved.ScheduledDeparture, saved.ScheduledArrival, incoming.ScheduledDeparture, incoming.ScheduledArrival},
		{"actual", saved.ActualDeparture, saved.ActualArrival, incoming.ActualDeparture, incoming.ActualArrival},
	} {
		// Avoid mixing endpoints from exports that disagree on an already
		// recorded time. The scheduled and actual pairs can fill independently.
		if passportTimeConflicts(pair.departure, pair.newDeparture) || passportTimeConflicts(pair.arrival, pair.newArrival) {
			continue
		}
		departure, arrival := pair.departure, pair.arrival
		if departure == nil {
			departure = pair.newDeparture
		}
		if arrival == nil {
			arrival = pair.newArrival
		}
		if departure != nil && arrival != nil && (!arrival.After(*departure) || arrival.Sub(*departure) > 48*time.Hour) {
			continue
		}
		if pair.departure == nil && departure != nil {
			patch[pair.prefix+"Departure"] = departure
		}
		if pair.arrival == nil && arrival != nil {
			patch[pair.prefix+"Arrival"] = arrival
		}
	}
	if len(patch) == 0 {
		return nil
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE passport_archive SET snapshot=json_patch(snapshot,?) WHERE owner=? AND id=?`, string(data), owner, incoming.ID)
	return err
}

func passportTimeConflicts(saved, incoming *time.Time) bool {
	return saved != nil && incoming != nil && !saved.Equal(*incoming)
}

func validPassportArchiveFlight(flight feeds.Flight, source string) bool {
	_, err := time.Parse(time.DateOnly, flight.HistoryDate)
	return err == nil && passportArchiveID.MatchString(flight.ID) && flight.Status == "arrived" && flight.Source == source
}

// UpdateManualPassportFlight replaces an owned manual entry only when its
// version matches. An identical retry succeeds without advancing the version.
func (s *Store) UpdateManualPassportFlight(ctx context.Context, owner string, flight feeds.Flight, expectedObservedAt time.Time) error {
	if !validPassportArchiveFlight(flight, feeds.PassportManualSource) || expectedObservedAt.IsZero() || flight.ObservedAt.IsZero() {
		return feeds.ErrInvalidQuery
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var saved []byte
	err = tx.QueryRowContext(ctx, `SELECT snapshot FROM passport_archive
WHERE owner=? AND id=? AND json_extract(snapshot,'$.source')=?`, owner, flight.ID, feeds.PassportManualSource).Scan(&saved)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var prior feeds.Flight
	if err := json.Unmarshal(saved, &prior); err != nil {
		return err
	}
	nextObservedAt := flight.ObservedAt.UTC()
	flight.ObservedAt = prior.ObservedAt
	snapshot, err := json.Marshal(flight)
	if err != nil {
		return err
	}
	if bytes.Equal(saved, snapshot) {
		return nil
	}
	if !prior.ObservedAt.Equal(expectedObservedAt) {
		return ErrPassportArchiveConflict
	}
	if !nextObservedAt.After(prior.ObservedAt) {
		nextObservedAt = prior.ObservedAt.Add(time.Nanosecond)
	}
	flight.ObservedAt = nextObservedAt
	snapshot, err = json.Marshal(flight)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE passport_archive SET flight_date=?,snapshot=? WHERE owner=? AND id=?`,
		flight.HistoryDate, string(snapshot), owner, flight.ID); err != nil {
		return err
	}
	return tx.Commit()
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
