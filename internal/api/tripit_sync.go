package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/itinerary"
)

// TripItSync holds only the operator key and timing policy. Per-installation
// feed URLs are encrypted with the owner as associated data.
type TripItSync struct {
	cipher   cipher.AEAD
	interval time.Duration
	client   *http.Client
	wake     chan struct{}
}

func NewTripItSync(key []byte, interval time.Duration) (*TripItSync, error) {
	if len(key) < 32 || interval < 5*time.Minute || interval > 24*time.Hour {
		return nil, errors.New("TripIt sync requires a key of at least 32 bytes and an interval between 5m and 24h")
	}
	derived := sha256.Sum256(append([]byte("plano-tripit-sync-v1\x00"), key...))
	block, err := aes.NewCipher(derived[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &TripItSync{cipher: aead, interval: interval, client: tripItClient, wake: make(chan struct{}, 1)}, nil
}

func (s Server) tripItSubscription(w http.ResponseWriter, r *http.Request, owner string) error {
	switch r.Method {
	case http.MethodGet:
		status, err := s.Store.TripItStatus(r.Context(), owner)
		if err != nil {
			return err
		}
		respond(w, 200, struct {
			core.TripItStatus
			Available bool `json:"available"`
		}{status, s.TripItSync != nil})
		return nil
	case http.MethodDelete:
		if err := s.Store.RemoveTripItSubscription(r.Context(), owner); err != nil {
			return err
		}
	case http.MethodPut:
		if s.TripItSync == nil {
			return feeds.ErrUnavailable
		}
		var input struct {
			URL string `json:"url"`
		}
		if err := parse(w, r, &input, 2048); err != nil {
			return err
		}
		target, err := tripItURL(input.URL)
		if err != nil {
			return err
		}
		secret := s.TripItSync.cipher.Seal(nil, nil, []byte(target.String()), []byte(owner))
		if err := s.Store.SetTripItSubscription(r.Context(), owner, secret, time.Now()); err != nil {
			return err
		}
		select {
		case s.TripItSync.wake <- struct{}{}:
		default:
		}
	}
	w.WriteHeader(204)
	return nil
}

// RunTripItSync claims one due installation at a time and holds no SQLite
// transaction while fetching a private calendar or resolving flights.
func (s Server) RunTripItSync(ctx context.Context) {
	if s.TripItSync == nil {
		return
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		job, err := s.Store.ClaimTripItSync(ctx, time.Now())
		if err != nil && ctx.Err() == nil {
			s.Logger.Error("could not claim a TripIt sync")
		}
		if err == nil && job != nil {
			s.syncTripItJob(ctx, *job)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.TripItSync.wake:
		}
	}
}

func (s Server) syncTripItJob(ctx context.Context, job core.TripItJob) {
	attempt, cancel := context.WithTimeout(ctx, 25*time.Second)
	result, more, err := s.fetchTripItJob(attempt, job, time.Now().UTC())
	cancel()
	now := time.Now().UTC()
	interval := s.TripItSync.interval
	if err != nil {
		result.Unavailable = true
	}
	if more {
		interval = min(interval, 2*time.Minute)
	}
	if err := s.Store.FinishTripItSync(ctx, job, result, now, now.Add(interval)); err != nil && ctx.Err() == nil {
		s.Logger.Error("could not commit a TripIt sync")
	}
}

func (s Server) fetchTripItJob(ctx context.Context, job core.TripItJob, now time.Time) (core.TripItSyncResult, bool, error) {
	result := core.TripItSyncResult{Cursor: job.Cursor}
	secret, err := s.TripItSync.cipher.Open(nil, nil, job.EncryptedURL, []byte(job.Owner))
	if err != nil {
		return result, false, feeds.ErrUnavailable
	}
	target, err := tripItURL(string(secret))
	if err != nil {
		return result, false, err
	}
	calendar, err := downloadTripIt(ctx, s.TripItSync.client, target)
	if err != nil {
		return result, false, err
	}
	from := now.AddDate(0, 0, -1).Format(time.DateOnly)
	parsed, err := itinerary.SubscriptionCalendar(calendar, from)
	if err != nil {
		return result, false, err
	}
	if parsed.Truncated {
		return result, false, feeds.ErrUnavailable
	}
	seen, err := s.Store.TripItImportedLegs(ctx, job.Owner, from)
	if err != nil {
		return result, false, err
	}
	legs := parsed.Legs
	sort.SliceStable(legs, func(i, j int) bool {
		a, b := legs[i], legs[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.Number != b.Number {
			return a.Number < b.Number
		}
		return a.UTCDeparture.Before(b.UTCDeparture)
	})
	return s.resolveTripItBatch(ctx, legs, seen, job.Cursor)
}

func tripItLegKey(leg itinerary.Leg) string {
	digest := sha256.Sum256([]byte(leg.Number + "\x00" + leg.Date + "\x00" + leg.UTCDeparture.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(digest[:])
}

func (s Server) resolveTripItBatch(ctx context.Context, legs []itinerary.Leg, seen map[string]bool, cursor int) (core.TripItSyncResult, bool, error) {
	result := core.TripItSyncResult{Cursor: cursor}
	if len(legs) == 0 {
		return result, false, nil
	}
	for _, leg := range legs {
		if seen[tripItLegKey(leg)] {
			result.Matched++
		}
	}
	checked, visited := 0, 0
	start := max(cursor, 0) % len(legs)
	for ; visited < len(legs) && checked < itinerary.MaxFlights; visited++ {
		leg := legs[(start+visited)%len(legs)]
		key := tripItLegKey(leg)
		if seen[key] {
			continue
		}
		if err := ctx.Err(); err != nil {
			result.Cursor = (start + visited) % len(legs)
			result.Unmatched = len(legs) - result.Matched
			return result, checked > 0, err
		}
		checked++
		matches, err := s.lookupLeg(ctx, leg)
		if err != nil {
			// Rotate past attempted legs even when a lookup uses the whole
			// budget. A slow flight should not starve later entries.
			result.Cursor = (start + visited + 1) % len(legs)
			result.Unmatched = len(legs) - result.Matched
			return result, ctx.Err() != nil && checked > 1, err
		}
		if len(matches) != 1 {
			continue
		}
		seen[key] = true
		result.Matches = append(result.Matches, core.TripItMatch{Key: key, Date: leg.Date, FlightID: matches[0].ID})
		result.Matched++
	}
	result.Cursor = (start + visited) % len(legs)
	result.Unmatched = len(legs) - result.Matched
	return result, visited < len(legs) && result.Unmatched > 0 && len(result.Matches) > 0, nil
}
