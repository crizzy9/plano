package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckConfigEnrollmentAndOfflineValidation(t *testing.T) {
	const enrollment = "fixture-enrollment-code"
	const aeroKey = "fixture-aeroapi-key"
	const invalidFlag = "fixture-invalid-flag"
	for _, tc := range []struct {
		name, demo, allowOpen, enrollment, aeroKey, addr, origin, wantError string
	}{
		{name: "demo without providers", demo: "true"},
		{name: "live requires enrollment before provider configuration", wantError: "PLANO_ENROLLMENT_CODE"},
		{name: "live with explicit closed enrollment", allowOpen: "false", aeroKey: aeroKey, wantError: "PLANO_ENROLLMENT_CODE"},
		{name: "protected live", enrollment: enrollment, aeroKey: aeroKey},
		{name: "explicit private enrollment", allowOpen: "true", aeroKey: aeroKey},
		{name: "invalid open enrollment flag", allowOpen: invalidFlag, enrollment: enrollment, aeroKey: aeroKey,
			wantError: "PLANO_ALLOW_OPEN_ENROLLMENT"},
		{name: "short enrollment code", enrollment: "fixture-short", aeroKey: aeroKey, wantError: "16 to 128 bytes"},
		{name: "invalid listen address", demo: "true", addr: "invalid listen address", wantError: "PLANO_ADDR"},
		{name: "public origin with empty query", demo: "true", origin: "https://plano.example?", wantError: "PLANO_PUBLIC_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateStartupEnvironment(t)
			t.Setenv("PLANO_DEMO", tc.demo)
			t.Setenv("PLANO_ALLOW_OPEN_ENROLLMENT", tc.allowOpen)
			t.Setenv("PLANO_ENROLLMENT_CODE", tc.enrollment)
			t.Setenv("PLANO_AEROAPI_KEY", tc.aeroKey)
			t.Setenv("PLANO_PUBLIC_URL", tc.origin)
			dbDir := filepath.Join(t.TempDir(), "absent")
			t.Setenv("PLANO_DB", filepath.Join(dbDir, "plano.db"))
			addr := tc.addr
			if addr == "" {
				addr = "127.0.0.1:0"
			}
			t.Setenv("PLANO_ADDR", addr)

			var logs bytes.Buffer
			err := run(slog.New(slog.NewJSONHandler(&logs, nil)), true)
			diagnostic := logs.String()
			if err != nil {
				diagnostic += err.Error()
			}
			for _, value := range []string{tc.enrollment, tc.aeroKey, invalidFlag} {
				if value != "" && strings.Contains(diagnostic, value) {
					t.Fatal("configuration diagnostic exposed a fixture value")
				}
			}
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("check-config failed: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("check-config error = %v, want diagnostic containing %q", err, tc.wantError)
			}
			if _, err := os.Stat(dbDir); !os.IsNotExist(err) {
				t.Fatalf("database parent should remain absent, stat error = %v", err)
			}
		})
	}
}

func isolateStartupEnvironment(t *testing.T) {
	t.Helper()
	// Preflight must only see fixture settings, including on an operator host.
	for _, name := range []string{
		"PLANO_PUBLIC_URL", "PLANO_ENROLLMENT_CODE_FILE", "PLANO_BOOKING_KEY_FILE", "PLANO_AIRPORT_TRANSFERS_FILE",
		"PLANO_AEROALERTS_KEY_FILE", "PLANO_ADVISORY_INGRESS_KEY_FILE", "PLANO_AEROAPI_KEY_FILE",
		"PLANO_METAR", "PLANO_AERO_SUBSCRIPTIONS", "PLANO_TRIPIT_SYNC_KEY_FILE", "PLANO_RESERVATION_GATEWAY_URL",
		"PLANO_FAA_NAS", "PLANO_SWIM_URL", "PLANO_EUROCONTROL_URL", "PLANO_ADSB_URL", "PLANO_FAA_REGISTRY_FILE",
		"PLANO_APNS_TEAM_ID", "PLANO_APNS_KEY_ID", "PLANO_APNS_BUNDLE_ID", "PLANO_APNS_PRIVATE_KEY_FILE", "PLANO_APNS_SANDBOX",
		"PLANO_FCM_PROJECT_ID", "PLANO_FCM_SERVICE_ACCOUNT_FILE", "PLANO_FCM_BANDWIDTH_CONSTRAINED",
		"PLANO_REFRESH_INTERVAL", "PLANO_DELAY_MODEL_FILE", "PLANO_COLLECT_DELAY_SAMPLES",
	} {
		t.Setenv(name, "")
	}
}
