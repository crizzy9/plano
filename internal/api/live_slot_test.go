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

	"github.com/crizzy9/plano/internal/api"
	"github.com/crizzy9/plano/internal/core"
)

func TestDeviceLiveSlotRequestValidation(t *testing.T) {
	store, err := core.Open(filepath.Join(t.TempDir(), "slots.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, err := store.CreateInstallation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	handler := (api.Server{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	start := core.Device{Platform: "ios", Kind: "live_start", Token: "fixture-start-token-for-test", LiveSlot: "start", LiveRevision: 1}
	alert := core.Device{Platform: "ios", Kind: "alert", Token: "fixture-alert-token-for-test", LiveRevision: 2}
	activity := "activity:" + strings.Repeat("a", 64)
	for _, tt := range []struct {
		name, method string
		device       core.Device
		want         int
	}{
		{"valid start", http.MethodPost, start, 204},
		{"slot cannot be used on delete", http.MethodDelete, start, 400},
		{"legacy exact delete", http.MethodDelete, core.Device{Platform: "ios", Kind: "live_start", Token: start.Token}, 204},
		{"legacy start", http.MethodPost, core.Device{Platform: "ios", Kind: "live_start", Token: start.Token}, 204},
		{"slot requires a positive revision", http.MethodPost, core.Device{Platform: "ios", Kind: "live_start", Token: start.Token, LiveSlot: "start"}, 400},
		{"revision requires a slot", http.MethodPost, core.Device{Platform: "ios", Kind: "live_start", Token: start.Token, LiveRevision: 1}, 400},
		{"activity slot cannot register a start", http.MethodPost, core.Device{Platform: "ios", Kind: "live_start", Token: start.Token, LiveSlot: activity, LiveRevision: 1}, 400},
		{"start slot cannot register an update", http.MethodPost, core.Device{Platform: "ios", Kind: "live_update", FlightID: "flight", Token: start.Token, LiveSlot: "start", LiveRevision: 1}, 400},
		{"update slot rejects uppercase hashes", http.MethodPost, core.Device{Platform: "ios", Kind: "live_update", FlightID: "flight", Token: start.Token, LiveSlot: "activity:" + strings.Repeat("A", 64), LiveRevision: 1}, 400},
		{"valid update requires a tracked flight", http.MethodPost, core.Device{Platform: "ios", Kind: "live_update", FlightID: "flight", Token: start.Token, LiveSlot: activity, LiveRevision: 1}, 404},
		{"slot cannot register an alert", http.MethodPost, core.Device{Platform: "ios", Kind: "alert", Token: start.Token, LiveSlot: "start", LiveRevision: 1}, 400},
		{"slot cannot register Android", http.MethodPost, core.Device{Platform: "android", Kind: "alert", Token: start.Token, LiveSlot: "start", LiveRevision: 1}, 400},
		{"iOS alert registration carries a revision", http.MethodPost, alert, 204},
		{"revision deletion requires an empty token and kind", http.MethodDelete, alert, 400},
		{"revision deletion requires iOS", http.MethodDelete, core.Device{Platform: "android", LiveRevision: 2}, 400},
		{"revision deletion requires a positive revision", http.MethodDelete, core.Device{Platform: "ios", LiveRevision: -2}, 400},
		{"iOS revision deletion", http.MethodDelete, core.Device{Platform: "ios", LiveRevision: 2}, 204},
		{"late slot registration is acknowledged", http.MethodPost, start, 204},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(tt.device)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(tt.method, "/v1/devices", bytes.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+owner)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tt.want, w.Body.String())
			}
			if tt.want == http.StatusNotFound {
				var failure struct {
					Error string `json:"error"`
				}
				if w.Header().Get("Content-Type") != "application/json" ||
					json.Unmarshal(w.Body.Bytes(), &failure) != nil || failure.Error != "record not found" {
					t.Fatalf("unexpected response for an untracked Live Activity: %s", w.Body.String())
				}
			}
		})
	}
}
