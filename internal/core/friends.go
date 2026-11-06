package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crizzy9/plano/internal/feeds"
)

const friendSchema = `
CREATE TABLE IF NOT EXISTS friend_invites (
  token_hash TEXT PRIMARY KEY, sharer TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  sharer_alias TEXT NOT NULL, expires_ns INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS friend_invites_expiry ON friend_invites(expires_ns);
CREATE TABLE IF NOT EXISTS friend_relationships (
  id TEXT PRIMARY KEY, sharer TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  viewer TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
  sharer_alias TEXT NOT NULL, viewer_alias TEXT NOT NULL,
  UNIQUE(sharer,viewer), UNIQUE(id,sharer), CHECK(sharer<>viewer)
);
CREATE INDEX IF NOT EXISTS friend_relationships_by_viewer ON friend_relationships(viewer);
CREATE TABLE IF NOT EXISTS friend_flights (
  friend_id TEXT NOT NULL, owner TEXT NOT NULL, flight_id TEXT NOT NULL,
  PRIMARY KEY(friend_id,flight_id),
  FOREIGN KEY(friend_id,owner) REFERENCES friend_relationships(id,sharer) ON DELETE CASCADE,
  FOREIGN KEY(owner,flight_id) REFERENCES tracked(owner,flight_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS friend_flights_by_tracking ON friend_flights(owner,flight_id);
CREATE INDEX IF NOT EXISTS friend_flights_by_flight ON friend_flights(flight_id,friend_id);
CREATE TABLE IF NOT EXISTS friend_alert_preferences (
  friend_id TEXT PRIMARY KEY REFERENCES friend_relationships(id) ON DELETE CASCADE,
  mode TEXT NOT NULL CHECK(mode IN ('off','arrival','all'))
);
CREATE TABLE IF NOT EXISTS outbox_friend_jobs (
  outbox_id INTEGER PRIMARY KEY REFERENCES outbox(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS outbox_friend_grants (
  outbox_id INTEGER NOT NULL REFERENCES outbox_friend_jobs(outbox_id) ON DELETE CASCADE,
  friend_id TEXT NOT NULL, flight_id TEXT NOT NULL,
  PRIMARY KEY(outbox_id,friend_id),
  FOREIGN KEY(friend_id,flight_id) REFERENCES friend_flights(friend_id,flight_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS outbox_friend_grants_by_friend ON outbox_friend_grants(friend_id,flight_id);
`

const (
	FriendAlertOff     = "off"
	FriendAlertArrival = "arrival"
	FriendAlertAll     = "all"
)

var (
	ErrInvalidFriend   = errors.New("invalid friends input")
	ErrFriendExists    = errors.New("friend relationship already exists")
	friendInviteCodeRE = regexp.MustCompile(`^[A-Z2-7]{32,128}$`)
)

type Friend struct {
	ID        string   `json:"id"`
	Alias     string   `json:"alias"`
	Direction string   `json:"direction"`
	FlightIDs []string `json:"flightIds"`
	AlertMode string   `json:"alertMode,omitempty"`
}

type FriendInvite struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// FriendFlight is only the provider flight snapshot. It never joins seat,
// booking, contact, installation, or device records into the response.
type FriendFlight struct {
	FriendID string       `json:"friendId"`
	Alias    string       `json:"alias"`
	Flight   feeds.Flight `json:"flight"`
}

type FriendsView struct {
	Friends []Friend       `json:"friends"`
	Flights []FriendFlight `json:"flights"`
}

func friendAlias(input string) (string, error) {
	alias := strings.TrimSpace(input)
	if alias == "" || !utf8.ValidString(alias) || utf8.RuneCountInString(alias) > 64 || strings.ContainsFunc(alias, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == '\u2028' || r == '\u2029'
	}) {
		return "", ErrInvalidFriend
	}
	return alias, nil
}

func (s *Store) CreateFriendInvite(ctx context.Context, sharer, name string, now time.Time) (FriendInvite, error) {
	alias, err := friendAlias(name)
	if err != nil {
		return FriendInvite{}, err
	}
	invite := FriendInvite{Code: randomToken(), ExpiresAt: now.UTC().Add(24 * time.Hour)}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FriendInvite{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM friend_invites WHERE expires_ns<=?`, now.UnixNano()); err != nil {
		return FriendInvite{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO friend_invites(token_hash,sharer,sharer_alias,expires_ns) VALUES(?,?,?,?)`, hash(invite.Code), sharer, alias, invite.ExpiresAt.UnixNano()); err != nil {
		return FriendInvite{}, err
	}
	if err := tx.Commit(); err != nil {
		return FriendInvite{}, err
	}
	return invite, nil
}

func (s *Store) AcceptFriendInvite(ctx context.Context, viewer, code, name string, now time.Time) (Friend, error) {
	alias, err := friendAlias(name)
	if err != nil {
		return Friend{}, err
	}
	code = strings.TrimSpace(code)
	if !friendInviteCodeRE.MatchString(code) {
		return Friend{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Friend{}, err
	}
	defer tx.Rollback()
	var sharer, sharerAlias string
	err = tx.QueryRowContext(ctx, `DELETE FROM friend_invites WHERE token_hash=? AND expires_ns>? RETURNING sharer,sharer_alias`, hash(code), now.UnixNano()).Scan(&sharer, &sharerAlias)
	if errors.Is(err, sql.ErrNoRows) {
		return Friend{}, ErrNotFound
	}
	if err != nil {
		return Friend{}, err
	}
	if sharer == viewer {
		return Friend{}, ErrInvalidFriend
	}
	friend := Friend{ID: rand.Text(), Alias: sharerAlias, Direction: "incoming", FlightIDs: make([]string, 0), AlertMode: FriendAlertOff}
	result, err := tx.ExecContext(ctx, `INSERT INTO friend_relationships(id,sharer,viewer,sharer_alias,viewer_alias)
VALUES(?,?,?,?,?) ON CONFLICT(sharer,viewer) DO NOTHING`, friend.ID, sharer, viewer, sharerAlias, alias)
	if err != nil {
		return Friend{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Friend{}, err
	}
	if count == 0 {
		return Friend{}, ErrFriendExists
	}
	if err := tx.Commit(); err != nil {
		return Friend{}, err
	}
	return friend, nil
}

func (s *Store) Friends(ctx context.Context, owner string) (FriendsView, error) {
	view := FriendsView{Friends: make([]Friend, 0), Flights: make([]FriendFlight, 0)}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,
CASE WHEN r.sharer=?1 THEN r.viewer_alias ELSE r.sharer_alias END,
CASE WHEN r.sharer=?1 THEN 'outgoing' ELSE 'incoming' END,
CASE WHEN r.viewer=?1 THEN COALESCE(p.mode,'off') ELSE '' END,
g.flight_id, CASE WHEN r.viewer=?1 THEN f.snapshot END
FROM friend_relationships r
LEFT JOIN friend_alert_preferences p ON p.friend_id=r.id
LEFT JOIN friend_flights g ON g.friend_id=r.id
LEFT JOIN flights f ON f.id=g.flight_id
WHERE r.sharer=?1 OR r.viewer=?1 ORDER BY r.id,g.flight_id`, owner)
	if err != nil {
		return view, err
	}
	defer rows.Close()
	for rows.Next() {
		var friend Friend
		var id sql.NullString
		var snapshot []byte
		if err := rows.Scan(&friend.ID, &friend.Alias, &friend.Direction, &friend.AlertMode, &id, &snapshot); err != nil {
			return view, err
		}
		if count := len(view.Friends); count == 0 || view.Friends[count-1].ID != friend.ID {
			friend.FlightIDs = make([]string, 0)
			view.Friends = append(view.Friends, friend)
		}
		if id.Valid {
			listed := &view.Friends[len(view.Friends)-1]
			listed.FlightIDs = append(listed.FlightIDs, id.String)
		}
		if len(snapshot) > 0 {
			var flight feeds.Flight
			if err := json.Unmarshal(snapshot, &flight); err != nil {
				return view, err
			}
			view.Flights = append(view.Flights, FriendFlight{FriendID: friend.ID, Alias: friend.Alias, Flight: flight})
		}
	}
	return view, rows.Err()
}

func (s *Store) SetFriendAlertMode(ctx context.Context, viewer, friendID, mode string) error {
	switch mode {
	case FriendAlertOff, FriendAlertArrival, FriendAlertAll:
	default:
		return ErrInvalidFriend
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(p.mode,'off') FROM friend_relationships r
LEFT JOIN friend_alert_preferences p ON p.friend_id=r.id WHERE r.id=? AND r.viewer=?`, friendID, viewer).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil || previous == mode {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO friend_alert_preferences(friend_id,mode) VALUES(?,?)
ON CONFLICT(friend_id) DO UPDATE SET mode=excluded.mode`, friendID, mode); err != nil {
		return err
	}
	// Each job retains only the grants that authorized it when it was queued.
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_friend_grants WHERE friend_id=?
AND EXISTS(SELECT 1 FROM outbox WHERE id=outbox_friend_grants.outbox_id AND done=0)`, friendID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GrantFriendFlight(ctx context.Context, owner, friendID, flightID string) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO friend_flights(friend_id,owner,flight_id)
SELECT r.id,r.sharer,t.flight_id FROM friend_relationships r
JOIN tracked t ON t.owner=r.sharer WHERE r.id=? AND r.sharer=? AND t.flight_id=?
ON CONFLICT(friend_id,flight_id) DO UPDATE SET flight_id=excluded.flight_id`, friendID, owner, flightID)
	return changedFriendRow(result, err)
}

func (s *Store) RevokeFriendFlight(ctx context.Context, owner, friendID, flightID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var present int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM friend_relationships WHERE id=? AND sharer=?`, friendID, owner).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM friend_flights WHERE friend_id=? AND owner=? AND flight_id=?`, friendID, owner, flightID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveFriend(ctx context.Context, owner, friendID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM friend_relationships WHERE id=? AND (sharer=? OR viewer=?)`, friendID, owner, owner)
	return changedFriendRow(result, err)
}

func changedFriendRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return ErrNotFound
	}
	return err
}
