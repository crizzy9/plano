package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
	"github.com/crizzy9/plano/internal/push"
	_ "github.com/mattn/go-sqlite3"
)

var ErrNotFound = errors.New("record not found")

type Store struct {
	db                     *sql.DB
	delayModel             *predict.Model
	earlyDelayModel        *predict.Model
	weatherForecast        *WeatherForecastCache
	collectDelaySamples    bool
	tfmsLegacyReplayCutoff int64
	outboxWake             chan struct{}
	refreshWake            chan struct{}
	apnsFlightAlerts       sync.Map
}

type Details struct {
	Flight           feeds.Flight     `json:"flight"`
	Events           []Event          `json:"events"`
	Telemetry        []feeds.Position `json:"telemetry"`
	ReceiverRevision string           `json:"receiverRevision,omitempty"`
	Inbound          []feeds.Flight   `json:"inbound"`
	Prediction       *Prediction      `json:"prediction"`
	Tracked          bool             `json:"-"`
}

type Device struct {
	Platform           string `json:"platform"`
	Token              string `json:"token"`
	Kind               string `json:"kind"`
	FlightID           string `json:"flightId,omitempty"`
	RequestUpdateToken bool   `json:"requestUpdateToken,omitempty"`
	Binding            string `json:"binding,omitempty"`
	LiveSlot           string `json:"liveSlot,omitempty"`
	LiveRevision       int64  `json:"liveRevision,omitempty"`
}

type StoreOptions struct {
	DelayModel          *predict.Model
	EarlyDelayModel     *predict.Model
	WeatherForecast     *WeatherForecastCache
	CollectDelaySamples bool
}

func Open(path string) (*Store, error) { return OpenWithOptions(path, StoreOptions{}) }

func OpenWithOptions(path string, options StoreOptions) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var legacyAdvisories bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='pushed_advisories')`).Scan(&legacyAdvisories); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err = db.Exec(`
CREATE TABLE IF NOT EXISTS installations (id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS flights (
  id TEXT PRIMARY KEY, snapshot TEXT NOT NULL, observed_ns INTEGER NOT NULL,
  telemetry TEXT NOT NULL DEFAULT '[]', inbound TEXT NOT NULL DEFAULT '[]', prediction TEXT NOT NULL DEFAULT 'null'
);
CREATE INDEX IF NOT EXISTS flights_by_inbound_id ON flights(json_extract(snapshot,'$.inboundId'))
WHERE json_extract(snapshot,'$.inboundId') IS NOT NULL;
CREATE INDEX IF NOT EXISTS flights_gate_history_route ON flights(
  json_extract(snapshot,'$.number'),json_extract(snapshot,'$.origin'),json_extract(snapshot,'$.destination')
);
CREATE INDEX IF NOT EXISTS flights_gate_history_operating_route ON flights(
  json_extract(snapshot,'$.icaoNumber'),json_extract(snapshot,'$.origin'),json_extract(snapshot,'$.destination')
) WHERE json_extract(snapshot,'$.source')='flightaware_aeroapi' AND json_extract(snapshot,'$.icaoNumber') IS NOT NULL;
CREATE TABLE IF NOT EXISTS flight_estimate_anchors (
  flight_id TEXT PRIMARY KEY REFERENCES flights(id) ON DELETE CASCADE,
  departure DATETIME, arrival DATETIME
);
CREATE TABLE IF NOT EXISTS pushed_advisories (
  source TEXT NOT NULL, id TEXT NOT NULL, observed_ns INTEGER NOT NULL,
  expires_ns INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(source,id)
);
CREATE INDEX IF NOT EXISTS pushed_advisories_expiry ON pushed_advisories(expires_ns);
CREATE INDEX IF NOT EXISTS pushed_advisories_airport ON pushed_advisories(json_extract(data,'$.airport'),expires_ns,observed_ns);
CREATE TABLE IF NOT EXISTS polled_flight_notices (
  source TEXT NOT NULL, id TEXT NOT NULL, airport TEXT NOT NULL, observed_ns INTEGER NOT NULL,
  expires_ns INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(source,id)
);
CREATE INDEX IF NOT EXISTS polled_flight_notices_airport ON polled_flight_notices(airport,expires_ns);
CREATE TABLE IF NOT EXISTS aero_instance_keys (
  provider_id TEXT NOT NULL, flight_id TEXT NOT NULL REFERENCES flights(id) ON DELETE CASCADE,
  PRIMARY KEY(provider_id,flight_id), UNIQUE(flight_id)
);
CREATE TABLE IF NOT EXISTS aero_refresh_jobs (
  provider_id TEXT PRIMARY KEY, generation INTEGER NOT NULL, due_ns INTEGER NOT NULL,
  lease_ns INTEGER NOT NULL DEFAULT 0, failures INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS aero_refresh_due ON aero_refresh_jobs(due_ns);
CREATE TABLE IF NOT EXISTS tracked (
  owner TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  flight_id TEXT NOT NULL REFERENCES flights(id), seat TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(owner, flight_id)
);
CREATE INDEX IF NOT EXISTS tracked_by_flight ON tracked(flight_id, owner);
CREATE TABLE IF NOT EXISTS devices (
  id INTEGER PRIMARY KEY AUTOINCREMENT, owner TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  platform TEXT NOT NULL, token TEXT NOT NULL, kind TEXT NOT NULL, flight_id TEXT NOT NULL,
  registered INTEGER NOT NULL, UNIQUE(platform, token, kind)
);
CREATE TABLE IF NOT EXISTS device_bindings (
  device_id INTEGER PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
  binding TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS device_live_slots (
  owner TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  slot TEXT NOT NULL, flight_id TEXT NOT NULL, revision INTEGER NOT NULL CHECK(revision>0),
  device_id INTEGER UNIQUE REFERENCES devices(id) ON DELETE SET NULL,
  PRIMARY KEY(owner,slot)
);
CREATE TABLE IF NOT EXISTS ios_device_floors (
  owner TEXT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
  revision INTEGER NOT NULL CHECK(revision>0)
);
CREATE TABLE IF NOT EXISTS ios_alert_revisions (
  device_id INTEGER PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
  revision INTEGER NOT NULL CHECK(revision>0)
);
CREATE TABLE IF NOT EXISTS ios_live_start_bindings (
  owner TEXT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE, binding TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY, flight_id TEXT NOT NULL REFERENCES flights(id), at_ns INTEGER NOT NULL, data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_by_flight ON events(flight_id, at_ns DESC);
CREATE TABLE IF NOT EXISTS flight_event_revisions (
  flight_id TEXT PRIMARY KEY REFERENCES flights(id) ON DELETE CASCADE, revision INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS event_revisions (
  event_id TEXT PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE, revision INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT, device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  flight_id TEXT NOT NULL REFERENCES flights(id), message TEXT NOT NULL,
  due INTEGER NOT NULL, expires INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
  lease_until INTEGER NOT NULL DEFAULT 0, done INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS outbox_ready ON outbox(due, id) WHERE done = 0;
CREATE INDEX IF NOT EXISTS outbox_ready_alerts ON outbox(due, id) WHERE done = 0 AND ` + pendingAlertKind + `;
CREATE INDEX IF NOT EXISTS outbox_by_device_flight ON outbox(device_id,flight_id,done,id);
CREATE INDEX IF NOT EXISTS outbox_pending_by_flight ON outbox(flight_id) WHERE done = 0;
CREATE TABLE IF NOT EXISTS outbox_live_start_leases (
  outbox_id INTEGER PRIMARY KEY REFERENCES outbox(id) ON DELETE CASCADE,
  may_have_sent INTEGER NOT NULL CHECK(may_have_sent IN (0,1))
);
CREATE TABLE IF NOT EXISTS live_wire_clocks (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  flight_id TEXT NOT NULL REFERENCES flights(id) ON DELETE CASCADE, wire_epoch INTEGER NOT NULL,
  PRIMARY KEY(device_id,flight_id)
);
CREATE TABLE IF NOT EXISTS live_token_requests (
  device_id INTEGER PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS check_in_queued (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  flight_id TEXT NOT NULL REFERENCES flights(id), PRIMARY KEY(device_id, flight_id)
);
CREATE TABLE IF NOT EXISTS outbox_check_in_leases (
  outbox_id INTEGER PRIMARY KEY REFERENCES outbox(id) ON DELETE CASCADE,
  lease_until INTEGER NOT NULL,
  may_have_sent INTEGER NOT NULL CHECK(may_have_sent IN (0,1))
);
` + checkInScheduleSchema + `
CREATE TABLE IF NOT EXISTS live_started (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE, flight_id TEXT NOT NULL REFERENCES flights(id),
  PRIMARY KEY(device_id, flight_id)
);
CREATE TABLE IF NOT EXISTS live_start_confirmations (
  device_id INTEGER NOT NULL, flight_id TEXT NOT NULL,
  PRIMARY KEY(device_id, flight_id),
  FOREIGN KEY(device_id, flight_id) REFERENCES live_started(device_id, flight_id) ON DELETE CASCADE
);
` + iosLiveStartSubmissionSchema + `
CREATE TABLE IF NOT EXISTS delay_samples (
  flight_id TEXT PRIMARY KEY REFERENCES flights(id), observed_ns INTEGER NOT NULL,
  scheduled_ns INTEGER NOT NULL, features TEXT NOT NULL, departed_ns INTEGER, label_observed_ns INTEGER
);
CREATE TABLE IF NOT EXISTS email_relays (
  owner TEXT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE, token_hash TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS imported_messages (
  owner TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  digest TEXT NOT NULL, imported INTEGER NOT NULL, PRIMARY KEY(owner, digest)
);
CREATE TABLE IF NOT EXISTS email_import_legs (
  owner TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  digest TEXT NOT NULL, leg_hash TEXT NOT NULL, flight_hash TEXT NOT NULL,
  PRIMARY KEY(owner, digest, leg_hash)
);
CREATE TABLE IF NOT EXISTS bookings (
  owner TEXT NOT NULL, flight_id TEXT NOT NULL, locator_hash TEXT NOT NULL,
  PRIMARY KEY(owner, flight_id, locator_hash),
  FOREIGN KEY(owner, flight_id) REFERENCES tracked(owner, flight_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS bookings_by_locator ON bookings(owner, locator_hash);
CREATE TABLE IF NOT EXISTS shares (
  token_hash TEXT PRIMARY KEY, owner TEXT NOT NULL, flight_id TEXT NOT NULL, expires INTEGER NOT NULL,
  FOREIGN KEY(owner, flight_id) REFERENCES tracked(owner, flight_id) ON DELETE CASCADE
);` + radarSchema + providerTrackSchema + pickupSchema + friendSchema + airportCacheSchema + passportArchiveSchema + tripItSyncSchema + aeroSubscriptionSchema + connectionMonitorSchema + outboxAdvisorySchema + advisorySupersessionSchema + delayProviderSamplesSchema + delayEarlySamplesSchema + tfmsProgramCancellationSchema + calendarSyncSchema + atfmSlotSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	tfmsLegacyReplayCutoff, err := upgradeTFMSAdvisoryDeadlines(db, time.Now(), legacyAdvisories)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{
		db: db, delayModel: options.DelayModel, earlyDelayModel: options.EarlyDelayModel,
		weatherForecast: options.WeatherForecast, collectDelaySamples: options.CollectDelaySamples,
		tfmsLegacyReplayCutoff: tfmsLegacyReplayCutoff, outboxWake: make(chan struct{}, 1), refreshWake: make(chan struct{}, 1),
	}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func randomToken() string { return rand.Text() + rand.Text() }

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *Store) CreateInstallation(ctx context.Context) (string, error) {
	token := randomToken()
	_, err := s.db.ExecContext(ctx, `INSERT INTO installations VALUES(?, ?)`, rand.Text(), hash(token))
	return token, err
}

func (s *Store) Installation(ctx context.Context, token string) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM installations WHERE token_hash = ?`, hash(token)).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return owner, err
}

func (s *Store) Track(ctx context.Context, owner, id, seat string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := trackFlight(ctx, tx, owner, id, seat, time.Now()); err != nil {
		return err
	}
	return s.commitOutbox(tx)
}

func trackFlight(ctx context.Context, tx *sql.Tx, owner, id, seat string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO tracked(owner, flight_id, seat)
SELECT ?, id, ? FROM flights WHERE id = ? ON CONFLICT(owner, flight_id) DO UPDATE SET seat = CASE WHEN excluded.seat = '' THEN tracked.seat ELSE excluded.seat END`, owner, seat, id)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count == 0 {
		return ErrNotFound
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT snapshot FROM flights WHERE id=?`, id).Scan(&raw); err != nil {
		return err
	}
	var flight feeds.Flight
	if err := json.Unmarshal(raw, &flight); err != nil {
		return err
	}
	return queueCheckIn(ctx, tx, flight, now)
}

func (s *Store) Untrack(ctx context.Context, owner, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Incoming Friends alerts are authorized by the sharer's grant, independently of this tracking row.
	if _, err = tx.ExecContext(ctx, `DELETE FROM outbox WHERE flight_id = ? AND device_id IN (SELECT id FROM devices WHERE owner = ?)
AND NOT EXISTS (SELECT 1 FROM outbox_friend_jobs WHERE outbox_id=outbox.id)`, id, owner); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM check_in_queued WHERE flight_id=? AND device_id IN (SELECT id FROM devices WHERE owner=?)`, id, owner); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM devices WHERE owner = ? AND kind = 'live_update' AND flight_id = ?`, owner, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM live_started WHERE flight_id = ? AND device_id IN (SELECT id FROM devices WHERE owner = ?)`, id, owner); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM tracked WHERE owner = ? AND flight_id = ?`, owner, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Tracked(ctx context.Context, owner string) ([]feeds.Flight, error) {
	return s.list(ctx, `SELECT f.snapshot FROM tracked t JOIN flights f ON t.flight_id=f.id WHERE t.owner=? ORDER BY f.observed_ns DESC LIMIT 1000`, owner)
}

func (s *Store) TrackedFlights(ctx context.Context, owner, firstID, secondID string) ([]feeds.Flight, error) {
	return s.list(ctx, `SELECT f.snapshot FROM tracked t JOIN flights f ON f.id=t.flight_id
WHERE t.owner=? AND t.flight_id IN (?,?) ORDER BY f.observed_ns DESC`, owner, firstID, secondID)
}

func (s *Store) Flight(ctx context.Context, id string) (feeds.Flight, error) {
	var flight feeds.Flight
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT snapshot FROM flights WHERE id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return flight, ErrNotFound
	}
	if err != nil {
		return flight, err
	}
	return flight, json.Unmarshal(raw, &flight)
}

func (s *Store) Active(ctx context.Context, now time.Time) ([]feeds.Flight, error) {
	return s.list(ctx, `SELECT f.snapshot FROM flights f WHERE EXISTS (SELECT 1 FROM tracked WHERE flight_id=f.id)
AND json_extract(f.snapshot, '$.scheduledDeparture') IS NOT NULL
AND julianday(json_extract(f.snapshot, '$.scheduledDeparture')) <= julianday(?)
AND julianday(COALESCE(json_extract(f.snapshot, '$.actualArrival'), json_extract(f.snapshot, '$.estimatedArrival'), json_extract(f.snapshot, '$.scheduledArrival'), json_extract(f.snapshot, '$.scheduledDeparture'))) >= julianday(?)
ORDER BY f.observed_ns`, now.Add(25*time.Hour).Format(time.RFC3339), now.Add(-6*time.Hour).Format(time.RFC3339))
}

func (s *Store) list(ctx context.Context, query string, args ...any) ([]feeds.Flight, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	flights := make([]feeds.Flight, 0)
	for rows.Next() {
		var raw []byte
		var flight feeds.Flight
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &flight); err != nil {
			return nil, err
		}
		flights = append(flights, flight)
	}
	return flights, rows.Err()
}

func (s *Store) Details(ctx context.Context, id string) (Details, error) {
	return s.DetailsForInstallation(ctx, "", id)
}

// DetailsForInstallation adds local receiver positions only while this
// installation tracks the flight. Provider history retains its existing scope.
func (s *Store) DetailsForInstallation(ctx context.Context, owner, id string) (Details, error) {
	d := Details{Events: make([]Event, 0)}
	var snapshot, telemetry, inbound, prediction []byte
	var observedNS, receiverRevision, eventRevision int64
	err := s.db.QueryRowContext(ctx, `SELECT snapshot,telemetry,inbound,prediction,observed_ns,
COALESCE((SELECT revision_ns FROM radar_receiver_revisions WHERE flight_id=flights.id),0),
COALESCE((SELECT revision FROM flight_event_revisions WHERE flight_id=flights.id),0),
EXISTS(SELECT 1 FROM tracked WHERE owner=? AND flight_id=flights.id) FROM flights WHERE id=?`, owner, id).
		Scan(&snapshot, &telemetry, &inbound, &prediction, &observedNS, &receiverRevision, &eventRevision, &d.Tracked)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	for _, field := range []struct {
		raw    []byte
		target any
	}{{snapshot, &d.Flight}, {telemetry, &d.Telemetry}, {inbound, &d.Inbound}, {prediction, &d.Prediction}} {
		if err := json.Unmarshal(field.raw, field.target); err != nil {
			return d, err
		}
	}
	if d.Prediction != nil {
		now := time.Now()
		validUntil, valid := predictionValidity(d.Prediction, d.Flight, d.Inbound, now)
		if !valid || !s.terminalWeatherForecastCurrent(d.Prediction, d.Flight, now) {
			d.Prediction = nil
		} else {
			d.Prediction.ValidUntil = validUntil
			revised, err := forecastAdvisoryRevised(ctx, s.db, d.Prediction, now)
			if err != nil {
				return d, err
			}
			if revised {
				d.Prediction = nil
			} else {
				d.Prediction.AdvisoryEvidence = nil
				d.Prediction.TerminalWeatherEvidence = nil
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.data FROM events e LEFT JOIN event_revisions r ON r.event_id=e.id
WHERE e.flight_id=? AND COALESCE(r.revision,0)=? AND (? OR json_extract(e.data,'$.kind') NOT IN ('inbound_takeoff','inbound_landing'))
ORDER BY e.at_ns DESC,e.id LIMIT 200`, id, eventRevision, d.Tracked)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var event Event
		if err := rows.Scan(&raw); err != nil {
			return d, err
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return d, err
		}
		d.Events = append(d.Events, event)
	}
	if err := rows.Err(); err != nil {
		return d, err
	}
	if err := rows.Close(); err != nil {
		return d, err
	}
	if d.Tracked {
		d.ReceiverRevision = strconv.FormatInt(receiverRevision, 10)
	}
	if d.Tracked && d.Flight.TailNumber != "" {
		// A concurrent observation can reset the receiver track after the detail
		// snapshot has been read. Keep newer fixes out of the older response.
		local, err := readRadarPositions(ctx, s.db, owner, id, nil, &observedNS, 2000, 0)
		if err != nil {
			return d, err
		}
		d.Telemetry = MergeFlightPositions(d.Telemetry, local)
	} else {
		d.Telemetry = slices.DeleteFunc(d.Telemetry, func(p feeds.Position) bool { return p.Source == "operator_readsb" })
	}
	return d, nil
}

func (s *Store) Observe(ctx context.Context, flight feeds.Flight, track []feeds.Position, inbound []feeds.Flight, advisories []feeds.Advisory) error {
	return s.observe(ctx, flight, track, inbound, advisories, observationDirect, aeroObservationScope{})
}

func (s *Store) observeTracked(ctx context.Context, flight feeds.Flight, track []feeds.Position, inbound []feeds.Flight, advisories []feeds.Advisory) error {
	return s.observe(ctx, flight, track, inbound, advisories, observationTracked, aeroObservationScope{})
}

type observationMode uint8

const (
	observationDirect observationMode = iota
	observationTracked
	observationPoll
)

func (s *Store) observe(ctx context.Context, flight feeds.Flight, track []feeds.Position, inbound []feeds.Flight, advisories []feeds.Advisory, mode observationMode, scope aeroObservationScope) error {
	if flight.ID == "" || flight.ObservedAt.IsZero() {
		return errors.New("missing flight identity or observation time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	trackedRefresh := mode != observationDirect
	callbackRefresh := scope.providerID != ""
	if callbackRefresh {
		var tracked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT id FROM (`+aeroTargetsSQL+`) WHERE id=?)`, scope.providerID, scope.providerID, flight.ID).Scan(&tracked); err != nil {
			return err
		}
		if !tracked {
			return nil
		}
	}
	var prior feeds.Flight
	var raw, oldInbound, oldPrediction, oldTrack []byte
	var estimateAnchors estimateAlertAnchors
	var radarState providerTrackState
	var previousProvider, lastRadarTail string
	var retainedDirect, providerChanged, receiverReset, refreshReceiver, providerScopeChanged bool
	provider := flight.AeroProviderID()
	observed := flight.ObservedAt.UnixNano()
	fetched := scope.providerTrackFetched
	if track != nil && provider != "" && mode != observationPoll {
		fetched = flight.ObservedAt
	}
	err = tx.QueryRowContext(ctx, `SELECT snapshot,inbound,prediction,telemetry,
COALESCE((SELECT provider_id FROM aero_instance_keys WHERE flight_id=flights.id),''),
COALESCE((SELECT last_tail FROM radar_receiver_revisions WHERE flight_id=flights.id),''), anchors.departure, anchors.arrival,
COALESCE(p.revision_ns,0),COALESCE(p.scope_ns,0),COALESCE(p.fetched_ns,0)
FROM flights LEFT JOIN flight_estimate_anchors anchors ON anchors.flight_id=flights.id
LEFT JOIN radar_provider_revisions p ON p.flight_id=flights.id
WHERE flights.id=? AND (NOT ? OR EXISTS(SELECT 1 FROM tracked WHERE flight_id=flights.id))`, flight.ID, mode == observationPoll).
		Scan(&raw, &oldInbound, &oldPrediction, &oldTrack, &previousProvider, &lastRadarTail, &estimateAnchors.departure, &estimateAnchors.arrival,
			&radarState.revision, &radarState.scope, &radarState.fetched)
	// Tracking can change while the poll is waiting on the provider.
	if mode == observationPoll && errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	priorRadarState := radarState
	if err == nil {
		if err := json.Unmarshal(raw, &prior); err != nil {
			return err
		}
		priorTrackProvider := previousProvider
		if priorTrackProvider == "" {
			priorTrackProvider = prior.AeroProviderID()
		}
		sameTrackIdentity := sameProviderTrackIdentity(prior, flight, priorTrackProvider, provider)
		sameTakeoff := sameOptionalInstant(prior.ActualTakeoff, flight.ActualTakeoff)
		sameTrackScope := sameTrackIdentity && sameTakeoff && strings.EqualFold(prior.TailNumber, flight.TailNumber)
		if !flight.ObservedAt.After(prior.ObservedAt) {
			if mode == observationPoll && sameTrackScope {
				return saveOverlappingProviderTrack(ctx, tx, prior, flight, track, oldTrack, radarState, fetched)
			}
			return nil
		}
		// A diversion continuation can reuse the original AeroAPI ID. A signed
		// snapshot with a different origin cannot replace a tracked departure.
		if scope.sameOrigin && flightAlertAirportChanged(prior.Origin, prior.OriginICAO, flight.Origin, flight.OriginICAO) {
			return nil
		}
		providerScopeChanged = (priorTrackProvider != "" || provider != "") && !sameTrackScope
		if providerScopeChanged {
			radarState.scope, radarState.fetched = observed, 0
			// A tail withdrawal fences pending writes while the known
			// provider path survives. Route and takeoff scope changes retire it.
			if priorTrackProvider != "" && prior.Source == "flightaware_aeroapi" && (!sameTrackIdentity || !sameTakeoff) {
				if track == nil {
					track = make([]feeds.Position, 0)
				}
				oldTrack = nil
			}
		} else if mode == observationPoll && !fetched.IsZero() && fetched.UnixNano() <= radarState.fetched {
			// The status can still advance independently of a superseded track fetch.
			track = nil
		}
		providerChanged = previousProvider != "" && provider != "" && provider != previousProvider
		if providerChanged || flight.TailNumber != prior.TailNumber || flight.InboundID != prior.InboundID {
			oldPrediction = []byte("null")
			if inbound == nil {
				inbound = make([]feeds.Flight, 0)
			}
		}
		if providerChanged {
			oldInbound = []byte("[]")
		}
		if prior.TailNumber != "" {
			lastRadarTail = prior.TailNumber
		}
		aircraftChanged := lastRadarTail != "" && flight.TailNumber != "" && lastRadarTail != flight.TailNumber
		tailWithdrawn := prior.TailNumber != "" && flight.TailNumber == ""
		refreshReceiver = prior.ActualTakeoff != nil && flight.ActualTakeoff != nil && flight.ActualTakeoff.After(*prior.ActualTakeoff)
		// A database from before receiver revisions can have fixes from an
		// earlier aircraft even when its last snapshot omitted the tail.
		tailRevealed := lastRadarTail == "" && flight.TailNumber != ""
		receiverReset = aircraftChanged || providerChanged || tailWithdrawn || tailRevealed
		if receiverReset {
			if !tailWithdrawn || providerChanged {
				lastRadarTail = flight.TailNumber
			}
			if err := reviseRadarReceiver(ctx, tx, flight, lastRadarTail, nil, true); err != nil {
				return err
			}
			if track == nil && (aircraftChanged || providerChanged || tailRevealed) {
				track = make([]feeds.Position, 0)
			}
			// Do not keep legacy receiver fixes attributed to the previous aircraft.
			if track == nil && len(oldTrack) > 0 {
				if err := json.Unmarshal(oldTrack, &track); err != nil {
					return err
				}
				track = slices.DeleteFunc(track, func(p feeds.Position) bool { return p.Source == "operator_readsb" })
			}
			oldTrack = nil
		}
		retainAircraftMetadata(&flight, prior, previousProvider)
		if inbound == nil {
			if err := json.Unmarshal(oldInbound, &inbound); err != nil {
				return err
			}
			retainedDirect = trackedRefresh && len(inbound) != 0 && isExactAeroInbound(flight, inbound[0])
		} else if trackedRefresh {
			recoverSuffix := scope.recoverRotationSuffix && samePolledRotationScope(prior, flight, previousProvider)
			inbound, retainedDirect, err = keepNewerAeroInbound(flight, inbound, oldInbound, recoverSuffix)
			if err != nil {
				return err
			}
		}

	}
	decisionAt := flight.ObservedAt
	for _, leg := range inbound {
		if leg.ObservedAt.After(decisionAt) {
			decisionAt = leg.ObservedAt
		}
	}
	var advisoryAt time.Time
	if mode == observationPoll && !retainedDirect {
		advisoryAt = time.Now()
		advisories, err = reconcilePolledFlightAdvisories(ctx, tx, flight, advisories, advisoryAt)
		if err != nil {
			return err
		}
		if decisionAt.Before(advisoryAt) {
			decisionAt = advisoryAt
		}
	}
	sameSchedule := !providerChanged && prior.ScheduledDeparture != nil && flight.ScheduledDeparture != nil && prior.ScheduledDeparture.Equal(*flight.ScheduledDeparture)
	lookupForecastChanged := !trackedRefresh && !callbackRefresh && sameSchedule &&
		flightAlertRelevant("delay_prediction", flight) && departureForecastInputsChanged(prior, flight)
	var predictionValue *Prediction
	var terminalForecasts []AirportWeatherForecast
	if trackedRefresh || prior.ID == "" || lookupForecastChanged || callbackRefresh && flightAlertRelevant("delay_prediction", flight) {
		forecastAdvisories := advisories
		var advisoryDeadlines map[forecastAdvisory]time.Time
		if retainedDirect || callbackRefresh || lookupForecastChanged && advisories == nil {
			// Callbacks supply flight data only. A retained direct leg may also
			// be newer than the poll's advisory snapshot. A lookup uses cached
			// notices for its forecast without replaying their alert events.
			if now := time.Now(); now.After(decisionAt) {
				decisionAt = now
			}
			forecastAdvisories, advisoryDeadlines, err = readCurrentFlightAdvisories(ctx, tx, flight, decisionAt)
			if err != nil {
				return err
			}
			if retainedDirect {
				advisories = forecastAdvisories
			}
		}
		terminalForecasts = s.weatherForecast.ForFlight(flight, decisionAt)
		predictionValue = predictWithTerminalWeather(s.delayModelFor(flight, decisionAt), flight, inbound, forecastAdvisories, terminalForecasts, decisionAt)
		capCachedAdvisoryValidity(predictionValue, advisoryDeadlines)
	}
	if !callbackRefresh && !lookupForecastChanged && sameSchedule && (!trackedRefresh || advisories == nil && predictionValue == nil) && len(oldPrediction) > 0 && flightBeforeDeparture(flight) {
		if err := json.Unmarshal(oldPrediction, &predictionValue); err != nil {
			return err
		}
		if predictionValue != nil {
			validUntil, valid := predictionValidity(predictionValue, flight, inbound, decisionAt)
			if !valid || trackedRefresh && usesInboundTiming(predictionValue) ||
				!s.terminalWeatherForecastCurrent(predictionValue, flight, decisionAt) {
				predictionValue = nil
			} else {
				predictionValue.ValidUntil = validUntil
			}
		}
	}
	if predictionValue != nil {
		if advisoryAt.IsZero() {
			advisoryAt = time.Now()
		}
		revised, err := forecastAdvisoryRevised(ctx, tx, predictionValue, advisoryAt)
		if err != nil {
			return err
		}
		if revised {
			predictionValue = nil
		}
	}

	cutoff := flight.ActualTakeoff
	mergeCachedReceiver := len(track) > 0 && len(oldTrack) > 0
	// The store serializes cached tracks with encoding/json. Avoid decoding a
	// provider-only cache when the observation supplies no replacement track.
	if mergeCachedReceiver || cutoff != nil && bytes.Contains(oldTrack, []byte(`"operator_readsb"`)) {
		var earlier []feeds.Position
		if err := json.Unmarshal(oldTrack, &earlier); err != nil {
			return err
		}
		if cutoff != nil {
			count := len(earlier)
			earlier = discardLegacyReceiverFixesBefore(earlier, *cutoff)
			if len(earlier) < count {
				refreshReceiver = true
				if track == nil {
					track = earlier
				}
			}
		}
		if mergeCachedReceiver {
			for _, p := range earlier {
				if p.Source == "operator_readsb" {
					track = append(track, p)
				}
			}
		}
	}
	if cutoff != nil {
		track = discardLegacyReceiverFixesBefore(track, *cutoff)
		if prior.ID != "" && !receiverReset {
			if err := reviseRadarReceiver(ctx, tx, flight, lastRadarTail, cutoff, refreshReceiver); err != nil {
				return err
			}
		}
	}
	if mergeCachedReceiver {
		sort.SliceStable(track, func(i, j int) bool { return track[i].At.Before(track[j].At) })
		track = slices.CompactFunc(track, func(a, b feeds.Position) bool {
			return a.At.Equal(b.At) && a.Latitude == b.Latitude && a.Longitude == b.Longitude
		})
	}
	if len(track) > 2000 {
		track = track[len(track)-2000:]
	}
	snapshot, err := json.Marshal(flight)
	if err != nil {
		return err
	}
	telemetry, err := optionalJSON(track)
	if err != nil {
		return err
	}
	rotation, err := optionalJSON(inbound)
	if err != nil {
		return err
	}
	prediction, err := json.Marshal(predictionValue)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO flights(id,snapshot,observed_ns,telemetry,inbound,prediction)
VALUES(?,?,?,COALESCE(?,'[]'),COALESCE(?,'[]'),?) ON CONFLICT(id) DO UPDATE SET
snapshot=excluded.snapshot, observed_ns=excluded.observed_ns, telemetry=COALESCE(?,flights.telemetry), inbound=COALESCE(?,flights.inbound),prediction=excluded.prediction`,
		flight.ID, string(snapshot), observed, telemetry, rotation, string(prediction), telemetry, rotation)
	if err != nil {
		return err
	}
	if telemetry != nil && !fetched.IsZero() && fetched.UnixNano() > radarState.fetched {
		radarState.fetched = fetched.UnixNano()
	}
	if radarState.revision >= observed && (providerScopeChanged || telemetry != nil && telemetry != string(oldTrack)) {
		radarState.revision++
	}
	if radarState != priorRadarState {
		if err := radarState.save(ctx, tx, flight.ID); err != nil {
			return err
		}
	}

	if provider != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO aero_instance_keys(provider_id,flight_id) VALUES(?,?)
ON CONFLICT(flight_id) DO UPDATE SET provider_id=excluded.provider_id`, provider, flight.ID); err != nil {
			return err
		}
	}
	if err := retireTrafficRadarScope(ctx, tx, prior, flight, previousProvider, provider); err != nil {
		return err
	}
	if previousProvider != "" || prior.AeroProviderID() != "" {
		if err := retireATFMSlotScope(ctx, tx, prior, flight, providerChanged); err != nil {
			return err
		}
	}
	if providerChanged {
		if _, err := tx.ExecContext(ctx, `INSERT INTO flight_event_revisions VALUES(?,1)
ON CONFLICT(flight_id) DO UPDATE SET revision=flight_event_revisions.revision+1`, flight.ID); err != nil {
			return err
		}
		if err := (estimateAlertAnchors{}).save(ctx, tx, flight.ID); err != nil {
			return err
		}
		if err := retireRemappedFlightJobs(ctx, tx, flight.ID); err != nil {
			return err
		}
	}

	if s.collectDelaySamples {
		if err := recordDelaySample(ctx, tx, flight, inbound, advisories, terminalForecasts, decisionAt, trackedRefresh); err != nil {
			return err
		}
	}

	events := make([]Event, 0, 8)
	var predictionCleared bool
	if prior.ID != "" {
		if !providerChanged {
			var nextAnchors estimateAlertAnchors
			events, nextAnchors = changes(prior, flight, estimateAnchors)
			if nextAnchors != estimateAnchors {
				if err := nextAnchors.save(ctx, tx, flight.ID); err != nil {
					return err
				}
			}
		}
		event, cleared, err := s.delayForecastTransition(ctx, tx, prior, flight, oldInbound, oldPrediction, predictionValue, decisionAt, sameSchedule)
		if err != nil {
			return err
		}
		predictionCleared = cleared
		if event.Kind != "" {
			events = append(events, event)
		}
	}
	if prior.ID != "" && !providerChanged {
		obsolete := supersededFlightAlertKinds(prior, flight, events)
		// A timer can queue check-in after this snapshot was captured. Check
		// for a moved opening at acceptance time as well.
		if checkInOpeningMovedLater(prior, flight, time.Now()) && !slices.Contains(obsolete, "check_in") {
			obsolete = append(obsolete, "check_in")
		}
		if predictionCleared && !slices.Contains(obsolete, "delay_prediction") {
			obsolete = append(obsolete, "delay_prediction")
		}
		if err := pruneSupersededFlightAlerts(ctx, tx, flight.ID, obsolete); err != nil {
			return err
		}
	}
	if err := queueCheckIn(ctx, tx, flight, flight.ObservedAt); err != nil {
		return err
	}
	for _, event := range events {
		if err := saveEvent(ctx, tx, flight.ID, event, flightAlertRelevant(event.Kind, flight)); err != nil {
			return err
		}
	}
	if !providerChanged && flight.InboundID != "" && prior.InboundID == flight.InboundID && len(inbound) != 0 {
		var previous []feeds.Flight
		if err := json.Unmarshal(oldInbound, &previous); err != nil {
			return err
		}
		if err := saveInboundMilestones(ctx, tx, flight, previous, inbound, decisionAt); err != nil {
			return err
		}
	}
	var cancellations []tfmsProgramCancellation
	if slices.ContainsFunc(advisories, feeds.Advisory.IsTFMSAirportProgram) {
		cancellations, err = loadTFMSProgramCancellations(ctx, tx, flight.Destination, flight.DestinationICAO, flight.ObservedAt)
		if err != nil {
			return err
		}
	}
	for _, a := range advisories {
		if event, ok := advisoryEvent(flight, a, flight.ObservedAt, cancellations...); ok {
			if err := saveEvent(ctx, tx, flight.ID, event, true); err != nil {
				return err
			}
		}
	}
	if err := enqueueLive(ctx, tx, prior, flight, providerChanged); err != nil {
		return err
	}
	return s.commitOutbox(tx)
}

func optionalJSON[T any](values []T) (any, error) {
	if values == nil {
		return nil, nil
	}
	raw, err := json.Marshal(values)
	return string(raw), err
}

func insertFlightEvent(ctx context.Context, tx *sql.Tx, flightID string, event *Event) (bool, error) {
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision FROM flight_event_revisions WHERE flight_id=?),0)`, flightID).Scan(&revision); err != nil {
		return false, err
	}
	if event.ID == "" {
		event.ID = hash(flightID + "\x00" + event.Kind + "\x00" + event.At.Format(time.RFC3339Nano))
	}
	if revision != 0 {
		event.ID = hash(event.ID + "\x00" + strconv.FormatInt(revision, 10))
	}
	data, err := json.Marshal(event)
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO events VALUES(?,?,?,?)`, event.ID, flightID, event.At.UnixNano(), string(data))
	if err != nil {
		return false, err
	}
	inserted, err := res.RowsAffected()
	if err != nil || inserted == 0 {
		return false, err
	}
	if revision != 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO event_revisions VALUES(?,?)`, event.ID, revision)
	}
	return true, err
}

func saveEvent(ctx context.Context, tx *sql.Tx, flightID string, event Event, notify bool) error {
	if inserted, err := insertFlightEvent(ctx, tx, flightID, &event); err != nil || !inserted || !notify {
		return err
	}
	expires := event.At.Add(2 * time.Hour)
	if event.Kind == "boarding" && event.collapseID == "" {
		expires = event.At.Add(boardingAlertLifetime)
	}
	if event.validUntil != nil {
		expires = earliestTime(expires, *event.validUntil)
	}
	if !expires.After(event.At) {
		return nil
	}
	collapseID := event.collapseID
	if collapseID == "" {
		collapseID = event.ID
	}
	msg := push.Message{Kind: push.Alert, FlightID: flightID, Title: event.Title, Body: truncate(event.Body, 180),
		Timestamp: event.At, ExpiresAt: expires, CollapseID: collapseID, AdvisoryWindow: event.advisoryWindow, InboundLegID: event.inboundLegID}
	if err := enqueueWithAdvisories(ctx, tx, flightID, msg, event.advisoryEvidence, `SELECT d.id FROM devices d JOIN tracked t ON t.owner=d.owner WHERE t.flight_id=? AND d.kind='alert'`, flightID); err != nil {
		return err
	}
	if event.inboundLegID != "" {
		return nil
	}
	return enqueueFriendFlightAlert(ctx, tx, flightID, msg, event)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xc0) == 0x80 {
		n--
	}
	return s[:n] + "…"
}

func (s *Store) Health(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) Shared(ctx context.Context, token string, now time.Time) (feeds.Flight, error) {
	var flight feeds.Flight
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT f.snapshot FROM shares s JOIN flights f ON f.id=s.flight_id WHERE s.token_hash=? AND s.expires>?`, hash(token), now.UnixNano()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return flight, ErrNotFound
	}
	if err != nil {
		return flight, err
	}
	return flight, json.Unmarshal(raw, &flight)
}
