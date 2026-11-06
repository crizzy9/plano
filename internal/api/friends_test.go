package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/api"
	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestFriendsDirectedFlightGrantsAndPickupLinkIsolation(t *testing.T) {
	store, err := core.Open(filepath.Join(t.TempDir(), "friends.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := t.Context()
	now := time.Now().UTC()
	flight := feeds.Flight{ID: "friend-fixture-1", Number: "UA7", Origin: "SFO", Destination: "EWR", Status: "scheduled", DepartureGate: "C10",
		ObservedAt: now, ScheduledDeparture: new(now.Add(2 * time.Hour)), ScheduledArrival: new(now.Add(7 * time.Hour))}
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	h := (api.Server{Store: store, PublicURL: "https://plano.example.com", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	call := func(method, path, token, body string, want int) []byte {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s = %d, want %d (%s)", method, path, w.Code, want, w.Body.String())
		}
		return w.Body.Bytes()
	}
	install := func() string {
		t.Helper()
		var response struct{ Token string }
		if err := json.Unmarshal(call("POST", "/v1/installations", "", `{}`, 201), &response); err != nil || response.Token == "" {
			t.Fatalf("installation token missing: %v", err)
		}
		return response.Token
	}
	invite := func(token, alias string) core.FriendInvite {
		t.Helper()
		var invite core.FriendInvite
		body, _ := json.Marshal(map[string]string{"alias": alias})
		if err := json.Unmarshal(call("POST", "/v1/friends/invites", token, string(body), 201), &invite); err != nil || invite.Code == "" {
			t.Fatalf("invalid friend invite: %v", err)
		}
		return invite
	}
	accept := func(token, code, alias string, want int) core.Friend {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"code": code, "alias": alias})
		raw := call("POST", "/v1/friends/accept", token, string(body), want)
		var response struct{ Friend core.Friend }
		if want == 201 {
			if err := json.Unmarshal(raw, &response); err != nil || response.Friend.ID == "" {
				t.Fatalf("missing relationship: %v", err)
			}
		}
		return response.Friend
	}
	view := func(token string) core.FriendsView {
		t.Helper()
		raw := call("GET", "/v1/friends", token, "", 200)
		for _, secret := range []string{`"seat"`, `"passenger"`, `"booking"`, "27C", "BOOK123"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("Friends exposed private field or data %s", secret)
			}
		}
		var response core.FriendsView
		if err := json.Unmarshal(raw, &response); err != nil || response.Friends == nil || response.Flights == nil {
			t.Fatalf("invalid Friends view: %v (%s)", err, raw)
		}
		return response
	}

	owner, viewer, outsider := install(), install(), install()
	call("GET", "/v1/friends", "", "", 401)
	call("POST", "/v1/friends/invites", owner, `{"alias":"\n"}`, 400)
	code := invite(owner, "Pilot").Code
	accept(owner, code, "Pilot", 400)
	friend := accept(viewer, code, "Pickup", 201)
	accept(outsider, code, "Unrelated", 404)
	if friend.Direction != "incoming" || friend.Alias != "Pilot" || friend.FlightIDs == nil || len(friend.FlightIDs) != 0 || friend.AlertMode != core.FriendAlertOff {
		t.Fatalf("accept response = %+v", friend)
	}
	if got := view(owner); len(got.Friends) != 1 || got.Friends[0].Direction != "outgoing" || got.Friends[0].Alias != "Pickup" || len(got.Flights) != 0 {
		t.Fatalf("sender view = %+v", got)
	}
	if got := view(outsider); len(got.Friends) != 0 || len(got.Flights) != 0 {
		t.Fatalf("unrelated view = %+v", got)
	}
	accept(viewer, invite(owner, "Pilot").Code, "Pickup", 409)

	base := "/v1/friends/" + friend.ID
	alerts := base + "/alerts"
	alertMode := func(token string) string {
		t.Helper()
		for _, entry := range view(token).Friends {
			if entry.ID == friend.ID {
				return entry.AlertMode
			}
		}
		t.Fatal("relationship missing from Friends view")
		return ""
	}
	if got := alertMode(viewer); got != core.FriendAlertOff {
		t.Fatalf("default alert mode = %q", got)
	}
	call("PUT", alerts, "", `{"mode":"all"}`, 401)
	call("PUT", alerts, owner, `{"mode":"all"}`, 404)
	call("PUT", alerts, outsider, `{"mode":"all"}`, 404)
	for _, body := range []string{`{}`, `{"mode":null}`, `{"mode":"unexpected"}`, `{"mode":"all","extra":true}`} {
		call("PUT", alerts, viewer, body, 400)
	}
	for _, mode := range []string{core.FriendAlertArrival, core.FriendAlertAll, core.FriendAlertAll, core.FriendAlertOff} {
		call("PUT", alerts, viewer, `{"mode":"`+mode+`"}`, 204)
		if got := alertMode(viewer); got != mode {
			t.Fatalf("viewer alert mode = %q, want %q", got, mode)
		}
		if got := alertMode(owner); got != "" {
			t.Fatalf("sender received viewer alert mode %q", got)
		}
	}
	grant := base + "/flights/" + flight.ID
	call("PUT", grant, owner, "", 404)
	call("POST", "/v1/tracked", owner, `{"flightId":"`+flight.ID+`","seat":"27C"}`, 204)
	ownerID, err := store.Installation(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitImport(ctx, ownerID, []feeds.Flight{flight}, "BOOK123-fixture-index", "", ""); err != nil {
		t.Fatal(err)
	}
	call("PUT", grant, viewer, "", 404)
	call("DELETE", grant, outsider, "", 404)
	call("PUT", grant, owner, "", 204)
	call("PUT", grant, owner, "", 204)
	if got := view(viewer); len(got.Flights) != 1 || got.Flights[0].FriendID != friend.ID || got.Flights[0].Alias != "Pilot" || got.Flights[0].Flight.ID != flight.ID || got.Flights[0].Flight.DepartureGate != "C10" ||
		len(got.Friends) != 1 || len(got.Friends[0].FlightIDs) != 1 || got.Friends[0].FlightIDs[0] != flight.ID {
		t.Fatalf("recipient flight view = %+v", got)
	}
	if got := view(owner); len(got.Flights) != 0 || len(got.Friends[0].FlightIDs) != 1 {
		t.Fatalf("sender grant view = %+v", got)
	}
	if got := view(outsider); len(got.Flights) != 0 {
		t.Fatalf("unrelated viewer received a flight: %+v", got)
	}

	flight.ObservedAt = now.Add(time.Second)
	flight.DepartureGate = "C12"
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := view(viewer); len(got.Flights) != 1 || got.Flights[0].Flight.DepartureGate != "C12" {
		t.Fatalf("shared flight did not reflect latest provider observation: %+v", got)
	}

	call("DELETE", grant, viewer, "", 404)
	call("DELETE", grant, owner, "", 204)
	call("DELETE", grant, owner, "", 204)
	if got := view(viewer); len(got.Flights) != 0 || len(got.Friends[0].FlightIDs) != 0 {
		t.Fatalf("revoked grant is visible: %+v", got)
	}
	call("PUT", grant, owner, "", 204)
	call("DELETE", "/v1/tracked/"+flight.ID, owner, "", 204)
	call("POST", "/v1/tracked", owner, `{"flightId":"`+flight.ID+`"}`, 204)
	if got := view(viewer); len(got.Flights) != 0 || len(got.Friends[0].FlightIDs) != 0 {
		t.Fatalf("untracked then retracked grant revived: %+v", got)
	}

	reverse := accept(owner, invite(viewer, "Pickup").Code, "Pilot", 201)
	if reverse.ID == friend.ID || reverse.Direction != "incoming" || reverse.Alias != "Pickup" {
		t.Fatalf("reverse consent = %+v", reverse)
	}
	call("PUT", grant, owner, "", 204)
	var pickup struct{ URL string }
	if err := json.Unmarshal(call("POST", "/v1/shares", owner, `{"flightId":"`+flight.ID+`"}`, 201), &pickup); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(pickup.URL)
	if err != nil || link.Host != "plano.example.com" {
		t.Fatalf("pickup link = %q, %v", pickup.URL, err)
	}
	call("DELETE", base, outsider, "", 404)
	call("DELETE", base, viewer, "", 204)
	call("PUT", alerts, viewer, `{"mode":"all"}`, 404)
	call("PUT", grant, owner, "", 404)
	if got := view(viewer); len(got.Flights) != 0 || len(got.Friends) != 1 || got.Friends[0].ID != reverse.ID {
		t.Fatalf("relationship removal leaked grant or removed reverse consent: %+v", got)
	}
	call("DELETE", "/v1/friends/"+reverse.ID, viewer, "", 204)
	if got := view(owner); len(got.Friends) != 0 || len(got.Flights) != 0 {
		t.Fatalf("sender could not remove inverse relationship: %+v", got)
	}
	call("GET", link.Path, "", "", 200)
}
