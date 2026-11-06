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
	untrack := httptest.NewRequest(http.MethodDelete, "/v1/tracked/"+flight.ID, nil)
	untrack.Header.Set("Authorization", "Bearer "+installationToken)
	untracked := httptest.NewRecorder()
	handler.ServeHTTP(untracked, untrack)
	if untracked.Code != http.StatusNoContent {
		t.Fatalf("untrack returned %d: %s", untracked.Code, untracked.Body.String())
	}

	// APNs can deliver a start after untracking. Its new slot has never
	// registered, even though it carries the same installation binding.
	late, err := json.Marshal(core.Device{Platform: "ios", Kind: "live_update", Token: token, Binding: binding,
		FlightID: flight.ID, LiveSlot: "activity:" + strings.Repeat("b", 64), LiveRevision: 3})
	if err != nil {
		t.Fatal(err)
	}
	update := httptest.NewRequest(http.MethodPost, "/v1/devices", bytes.NewReader(late))
	update.Header.Set("Authorization", "Bearer "+installationToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, update)
	var problem struct {
		Error string `json:"error"`
	}
	if response.Code != http.StatusNotFound || response.Header().Get("Content-Type") != "application/json" ||
		json.Unmarshal(response.Body.Bytes(), &problem) != nil || problem.Error != "record not found" {
		t.Fatalf("late live update returned %d: %s", response.Code, response.Body.String())
	}
}
