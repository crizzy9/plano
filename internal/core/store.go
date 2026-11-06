package core

import (
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
	"sync"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
	"github.com/crizzy9/plano/internal/push"
	_ "github.com/mattn/go-sqlite3"
)

var ErrNotFound = errors.New("record not found")

type Store struct {
	db                  *sql.DB
	delayModel          *predict.Model
	collectDelaySamples bool
	outboxWake          chan struct{}
	refreshWake         chan struct{}
	apnsFlightAlerts    sync.Map
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
	if _, err = db.Exec(`
CREATE TABLE IF NOT EXISTS installations (id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS flights (
  id TEXT PRIMARY KEY, snapshot TEXT NOT NULL, observed_ns INTEGER NOT NULL,
  telemetry TEXT NOT NULL DEFAULT '[]', inbound TEXT NOT NULL DEFAULT '[]', prediction TEXT NOT NULL DEFAULT 'null'
);
CREATE INDEX IF NOT EXISTS flights_by_inbound_id ON flights(json_extract(snapshot,'$.inboundId'))
WHERE json_extract(snapshot,'$.inboundId') IS NOT NULL;
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
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY, flight_id TEXT NOT NULL REFERENCES flights(id), at_ns INTEGER NOT NULL, data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_by_flight ON events(flight_id, at_ns DESC);
CREATE TABLE IF NOT EXISTS outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT, device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  flight_id TEXT NOT NULL REFERENCES flights(id), message TEXT NOT NULL,
  due INTEGER NOT NULL, expires INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
  lease_until INTEGER NOT NULL DEFAULT 0, done INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS outbox_ready ON outbox(due, id) WHERE done = 0;
CREATE INDEX IF NOT EXISTS outbox_by_device_flight ON outbox(device_id,flight_id,done,id);
CREATE INDEX IF NOT EXISTS outbox_pending_by_flight ON outbox(flight_id) WHERE done = 0;
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
CREATE TABLE IF NOT EXISTS live_started (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE, flight_id TEXT NOT NULL REFERENCES flights(id),
  PRIMARY KEY(device_id, flight_id)
);
CREATE TABLE IF NOT EXISTS live_start_confirmations (
  device_id INTEGER NOT NULL, flight_id TEXT NOT NULL,
  PRIMARY KEY(device_id, flight_id),
  FOREIGN KEY(device_id, flight_id) REFERENCES live_started(device_id, flight_id) ON DELETE CASCADE
);
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
CREATE TABLE IF NOT EXISTS bookings (
  owner TEXT NOT NULL, flight_id TEXT NOT NULL, locator_hash TEXT NOT NULL,
  PRIMARY KEY(owner, flight_id, locator_hash),
  FOREIGN KEY(owner, flight_id) REFERENCES tracked(owner, flight_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS bookings_by_locator ON bookings(owner, locator_hash);
CREATE TABLE IF NOT EXISTS shares (
  token_hash TEXT PRIMARY KEY, owner TEXT NOT NULL, flight_id TEXT NOT NULL, expires INTEGER NOT NULL,
  FOREIGN KEY(owner, flight_id) REFERENCES tracked(owner, flight_id) ON DELETE CASCADE
);` + radarSchema + pickupSchema + friendSchema + airportCacheSchema + tripItSyncSchema + aeroSubscriptionSchema + connectionMonitorSchema + outboxAdvisorySchema + advisorySupersessionSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, delayModel: options.DelayModel, collectDelaySamples: options.CollectDelaySamples, outboxWake: make(chan struct{}, 1), refreshWake: make(chan struct{}, 1)}, nil
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
	if _, err = tx.ExecContext(ctx, `DELETE FROM outbox WHERE flight_id = ? AND device_id IN (SELECT id FROM devices WHERE owner = ?)`, id, owner); err != nil {
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
	var observedNS, receiverRevision int64
	err := s.db.QueryRowContext(ctx, `SELECT snapshot,telemetry,inbound,prediction,observed_ns,
COALESCE((SELECT revision_ns FROM radar_receiver_revisions WHERE flight_id=flights.id),0),
EXISTS(SELECT 1 FROM tracked WHERE owner=? AND flight_id=flights.id) FROM flights WHERE id=?`, owner, id).
		Scan(&snapshot, &telemetry, &inbound, &prediction, &observedNS, &receiverRevision, &d.Tracked)
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
		if !valid {
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
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM events WHERE flight_id=? ORDER BY at_ns DESC,id LIMIT 200`, id)
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
		local, err := readRadarPositions(ctx, s.db, owner, id, nil, &observedNS, 2000)
		if err != nil {
			return d, err
		}
		d.Telemetry = mergeFlightPositions(d.Telemetry, local)
	} else {
		d.Telemetry = slices.DeleteFunc(d.Telemetry, func(p feeds.Position) bool { return p.Source == "operator_readsb" })
	}
	return d, nil
}

func (s *Store) Observe(ctx context.Context, flight feeds.Flight, track []feeds.Position, inbound []feeds.Flight, advisories []feeds.Advisory) error {
	return s.observe(ctx, flight, track, inbound, advisories, false, "")
}

func (s *Store) observeTracked(ctx context.Context, flight feeds.Flight, track []feeds.Position, inbound []feeds.Flight, advisories []feeds.Advisory) error {
	return s.observe(ctx, flight, track, inbound, advisories, true, "")
}

func (s *Store) observe(ctx context.Context, flight feeds.Flight, track []feeds.Position, inbound []feeds.Flight, advisories []feeds.Advisory, trackedRefresh bool, expectedAeroID string) error {
	if flight.ID == "" || flight.ObservedAt.IsZero() {
		return errors.New("missing flight identity or observation time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if expectedAeroID != "" {
		var tracked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT id FROM (`+aeroTargetsSQL+`) WHERE id=?)`, expectedAeroID, expectedAeroID, flight.ID).Scan(&tracked); err != nil {
			return err
		}
		if !tracked {
			return nil
		}
	}
	var prior feeds.Flight
	var raw, oldInbound, oldPrediction, oldTrack []byte
	var estimateAnchors estimateAlertAnchors
	var previousProvider, lastRadarTail string
	var retainedDirect bool
	provider := flight.AeroProviderID()
	err = tx.QueryRowContext(ctx, `SELECT snapshot,inbound,prediction,telemetry,
COALESCE((SELECT provider_id FROM aero_instance_keys WHERE flight_id=flights.id),''),
COALESCE((SELECT last_tail FROM radar_receiver_revisions WHERE flight_id=flights.id),''), anchors.departure, anchors.arrival
FROM flights LEFT JOIN flight_estimate_anchors anchors ON anchors.flight_id=flights.id WHERE flights.id=?`, flight.ID).
		Scan(&raw, &oldInbound, &oldPrediction, &oldTrack, &previousProvider, &lastRadarTail, &estimateAnchors.departure, &estimateAnchors.arrival)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &prior); err != nil {
			return err
		}
		if !flight.ObservedAt.After(prior.ObservedAt) {
			return nil
		}
		if flight.TailNumber != prior.TailNumber || flight.InboundID != prior.InboundID {
			oldPrediction = []byte("null")
			if inbound == nil {
				inbound = make([]feeds.Flight, 0)
			}
		}
		providerChanged := previousProvider != "" && provider != "" && provider != previousProvider
		if prior.TailNumber != "" {
			lastRadarTail = prior.TailNumber
		}
		aircraftChanged := lastRadarTail != "" && flight.TailNumber != "" && lastRadarTail != flight.TailNumber
		tailWithdrawn := prior.TailNumber != "" && flight.TailNumber == ""
		// A database from before receiver revisions can have fixes from an
		// earlier aircraft even when its last snapshot omitted the tail.
		tailRevealed := lastRadarTail == "" && flight.TailNumber != ""
		if aircraftChanged || providerChanged || tailWithdrawn || tailRevealed {
			if !tailWithdrawn || providerChanged {
				lastRadarTail = flight.TailNumber
			}
			if err := resetRadarReceiver(ctx, tx, flight, lastRadarTail); err != nil {
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
			inbound, retainedDirect, err = keepNewerAeroInbound(flight, inbound, oldInbound)
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
	var predictionValue *Prediction
	if trackedRefresh || prior.ID == "" {
		var advisoryDeadlines map[forecastAdvisory]time.Time
		if retainedDirect {
			// A saved direct leg may have been refreshed during the network
			// requests. Use advisory revisions current at the decision time.
			if now := time.Now(); now.After(decisionAt) {
				decisionAt = now
			}
			advisories, advisoryDeadlines, err = readCurrentFlightAdvisories(ctx, tx, flight, decisionAt)
			if err != nil {
				return err
			}
		}
		predictionValue = PredictWithModel(s.delayModel, flight, inbound, advisories, decisionAt)
		capCachedAdvisoryValidity(predictionValue, advisoryDeadlines)
	}
	sameSchedule := prior.ScheduledDeparture != nil && flight.ScheduledDeparture != nil && prior.ScheduledDeparture.Equal(*flight.ScheduledDeparture)
	if sameSchedule && (!trackedRefresh || advisories == nil && predictionValue == nil) && len(oldPrediction) > 0 && flightBeforeDeparture(flight) {
		if err := json.Unmarshal(oldPrediction, &predictionValue); err != nil {
			return err
		}
		if predictionValue != nil {
			validUntil, valid := predictionValidity(predictionValue, flight, inbound, decisionAt)
			if !valid || trackedRefresh && usesInboundTiming(predictionValue) {
				predictionValue = nil
			} else {
				predictionValue.ValidUntil = validUntil
			}
		}
	}
	if predictionValue != nil {
		revised, err := forecastAdvisoryRevised(ctx, tx, predictionValue, time.Now())
		if err != nil {
			return err
		}
		if revised {
			predictionValue = nil
		}
	}

	if len(track) > 0 && len(oldTrack) > 0 {
		var earlier []feeds.Position
		if err := json.Unmarshal(oldTrack, &earlier); err != nil {
			return err
		}
		for _, p := range earlier {
			if p.Source == "operator_readsb" {
				track = append(track, p)
			}
		}
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
		flight.ID, string(snapshot), flight.ObservedAt.UnixNano(), telemetry, rotation, string(prediction), telemetry, rotation)
	if err != nil {
		return err
	}

	if provider != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO aero_instance_keys(provider_id,flight_id) VALUES(?,?)
ON CONFLICT(flight_id) DO UPDATE SET provider_id=excluded.provider_id`, provider, flight.ID); err != nil {
			return err
		}
	}

	if s.collectDelaySamples && (trackedRefresh || expectedAeroID != "" && flight.ActualDeparture != nil) {
		if err := recordDelaySample(ctx, tx, flight, inbound, advisories, decisionAt); err != nil {
			return err
		}
	}

	events := make([]Event, 0, 8)
	var predictionCleared bool
	if prior.ID != "" {
		var nextAnchors estimateAlertAnchors
		events, nextAnchors = changes(prior, flight, estimateAnchors)
		if nextAnchors != estimateAnchors {
			if err := nextAnchors.save(ctx, tx, flight.ID); err != nil {
				return err
			}
		}
		event, cleared, err := delayForecastTransition(ctx, tx, prior, flight, oldInbound, oldPrediction, predictionValue, decisionAt, sameSchedule)
		if err != nil {
			return err
		}
		predictionCleared = cleared
		if event.Kind != "" {
			events = append(events, event)
		}
	}
	if prior.ID != "" {
		obsolete := supersededFlightAlertKinds(prior, flight, events)
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
	for _, a := range advisories {
		if event, ok := advisoryEvent(flight, a, flight.ObservedAt); ok {
			if err := saveEvent(ctx, tx, flight.ID, event, true); err != nil {
				return err
			}
		}
	}
	if err := enqueueLive(ctx, tx, prior, flight); err != nil {
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

func saveEvent(ctx context.Context, tx *sql.Tx, flightID string, event Event, notify bool) error {
	if event.ID == "" {
		event.ID = hash(flightID + "\x00" + event.Kind + "\x00" + event.At.Format(time.RFC3339Nano))
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO events VALUES(?,?,?,?)`, event.ID, flightID, event.At.UnixNano(), string(data))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 || !notify {
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
		Timestamp: event.At, ExpiresAt: expires, CollapseID: collapseID, AdvisoryWindow: event.advisoryWindow}
	return enqueueWithAdvisories(ctx, tx, flightID, msg, event.advisoryEvidence, `SELECT d.id FROM devices d JOIN tracked t ON t.owner=d.owner WHERE t.flight_id=? AND d.kind='alert'`, flightID)
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
