package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crizzy9/plano/internal/api"
	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/demodata"
)

func TestSelfHostedTrackingPersistsAndSharesStayScoped(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plano.db")
	store, err := core.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := func() http.Handler {
		return api.Server{Store: store, Source: demodata.Source{}, PublicURL: "https://flights.example.org", Demo: true, Logger: logger}.Handler()
	}
	h := handler()
	call := func(method, path, token, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: got %d, want %d (%s)", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	token := func() string {
		t.Helper()
		var installation struct{ Token string }
		if err := json.Unmarshal(call("POST", "/v1/installations", "", "{}", 201).Body.Bytes(), &installation); err != nil || installation.Token == "" {
			t.Fatalf("invalid installation: %v", err)
		}
		return installation.Token
	}
	owner, other := token(), token()
	call("GET", "/healthz", "", "", 200)
	call("GET", "/v1/tracked", "", "", 401)
	const id = "demo-UA123-2020-01-01"
	const track = `{"flightId":"` + id + `"}`
	search := call("GET", "/v1/flights?number=UA123&date=2020-01-01", owner, "", 200)
	if !strings.Contains(search.Body.String(), id) {
		t.Fatal(search.Body.String())
	}
	call("POST", "/v1/tracked", owner, track, 204)
	call("POST", "/v1/shares", other, track, 404)
	var share struct{ URL string }
	if err := json.Unmarshal(call("POST", "/v1/shares", owner, track, 201).Body.Bytes(), &share); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(share.URL)
	if err != nil || link.Host != "flights.example.org" {
		t.Fatalf("invalid share origin: %q", share.URL)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = core.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	h = handler()
	if tracked := call("GET", "/v1/tracked", owner, "", 200); !strings.Contains(tracked.Body.String(), id) {
		t.Fatal(tracked.Body.String())
	}
	if tracked := call("GET", "/v1/tracked", other, "", 200); strings.Contains(tracked.Body.String(), id) {
		t.Fatal("cross-installation tracking leak")
	}
	if passport := call("GET", "/v1/passport", owner, "", 200); !strings.Contains(passport.Body.String(), `"miles":2565`) {
		t.Fatal(passport.Body.String())
	}
	if page := call("GET", link.Path, "", "", 200); !strings.Contains(page.Body.String(), "SFO to EWR") || !strings.Contains(page.Body.String(), "Synthetic demo flight") {
		t.Fatal(page.Body.String())
	}
	call("DELETE", "/v1/tracked/"+id, owner, "", 204)
	call("GET", link.Path, "", "", 404)
}
