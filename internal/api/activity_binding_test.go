package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/api"
	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestActivityBindingRequestValidation(t *testing.T) {
	ctx := t.Context()
	store, err := core.Open(filepath.Join(t.TempDir(), "bindings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	installationToken, err := store.CreateInstallation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Installation(ctx, installationToken)
	if err != nil {
		t.Fatal(err)
	}
	flight := feeds.Flight{ID: "fixture-flight", ObservedAt: time.Now(), Status: "scheduled"}
	if err := store.Observe(ctx, flight, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, owner, flight.ID, ""); err != nil {
		t.Fatal(err)
	}
	handler := (api.Server{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	const binding = "0af06679-3c08-4f26-9a5b-15f31d4904ab"
	const token = "fixture-activity-token-for-test"
	activity := "activity:" + strings.Repeat("a", 64)
	for _, test := range []struct {
		name, method string
		device       core.Device
		want         int
	}{
		{"bound start", http.MethodPost, core.Device{Platform: "ios", Kind: "live_start", Token: token, Binding: binding, LiveSlot: "start", LiveRevision: 1}, 204},
		{"bound update", http.MethodPost, core.Device{Platform: "ios", Kind: "live_update", Token: token, Binding: binding, FlightID: flight.ID, LiveSlot: activity, LiveRevision: 2}, 204},
		{"bound alert rejected", http.MethodPost, core.Device{Platform: "ios", Kind: "alert", Token: token, Binding: binding}, 400},
		{"unscoped binding rejected", http.MethodPost, core.Device{Platform: "ios", Kind: "live_start", Token: token, Binding: binding}, 400},
		{"malformed binding rejected", http.MethodPost, core.Device{Platform: "ios", Kind: "live_start", Token: token, Binding: strings.ToUpper(binding), LiveSlot: "start", LiveRevision: 3}, 400},
		{"iOS binding-only deletion rejected", http.MethodDelete, core.Device{Platform: "ios", Kind: "live_start", Binding: binding}, 400},
		{"Android alert binding", http.MethodPost, core.Device{Platform: "android", Kind: "alert", Token: token, Binding: binding}, 204},
		{"Android binding-only deletion", http.MethodDelete, core.Device{Platform: "android", Kind: "alert", Binding: binding}, 204},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.device)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(test.method, "/v1/devices", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+installationToken)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}
