package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/predict"
)

func TestCheckConfigEnrollmentAndOfflineValidation(t *testing.T) {
	const enrollment = "fixture-enrollment-code"
	const aeroKey = "fixture-aeroapi-key"
	const aeroCallbackKey = "fixture-callback-key-not-production"
	const invalidFlag = "fixture-invalid-flag"
	for _, tc := range []struct {
		name, demo, allowOpen, enrollment, aeroKey, addr, origin, wantError string
		subscriptions, includeInbound, requireEnrollment, taf               string
		withAeroCallback                                                    bool
	}{
		{name: "demo without providers", demo: "true"},
		{name: "public demo requires enrollment", demo: "true", requireEnrollment: "true", wantError: "PLANO_ENROLLMENT_CODE"},
		{name: "protected demo without providers", demo: "true", requireEnrollment: "true", enrollment: enrollment},
		{name: "public deployment cannot enable open enrollment", requireEnrollment: "true", allowOpen: "true", aeroKey: aeroKey,
			wantError: "PLANO_ENROLLMENT_CODE"},
		{name: "invalid require enrollment flag", demo: "true", requireEnrollment: invalidFlag, wantError: "PLANO_REQUIRE_ENROLLMENT"},
		{name: "live requires enrollment before provider configuration", wantError: "PLANO_ENROLLMENT_CODE"},
		{name: "live with explicit closed enrollment", allowOpen: "false", aeroKey: aeroKey, wantError: "PLANO_ENROLLMENT_CODE"},
		{name: "protected live", enrollment: enrollment, aeroKey: aeroKey},
		{name: "live with terminal forecasts", enrollment: enrollment, aeroKey: aeroKey, taf: "true"},
		{name: "invalid terminal forecast flag", enrollment: enrollment, aeroKey: aeroKey, taf: invalidFlag, wantError: "PLANO_TAF"},
		{name: "demo skips terminal forecast settings", demo: "true", taf: invalidFlag},
		{name: "explicit private enrollment", allowOpen: "true", aeroKey: aeroKey},
		{name: "invalid open enrollment flag", allowOpen: invalidFlag, enrollment: enrollment, aeroKey: aeroKey,
			wantError: "PLANO_ALLOW_OPEN_ENROLLMENT"},
		{name: "short enrollment code", enrollment: "fixture-short", aeroKey: aeroKey, wantError: "16 to 128 bytes"},
		{name: "invalid listen address", demo: "true", addr: "invalid listen address", wantError: "PLANO_ADDR"},
		{name: "public origin with empty query", demo: "true", origin: "https://plano.example?", wantError: "PLANO_PUBLIC_URL"},
		{name: "invalid inbound subscription flag", enrollment: enrollment, aeroKey: aeroKey, includeInbound: invalidFlag,
			wantError: "PLANO_AERO_SUBSCRIPTIONS_INBOUND"},
		{name: "inbound subscriptions require managed subscriptions", enrollment: enrollment, aeroKey: aeroKey, includeInbound: "true",
			wantError: "PLANO_AERO_SUBSCRIPTIONS_INBOUND requires PLANO_AERO_SUBSCRIPTIONS=true"},
		{name: "inbound subscriptions require a callback key", enrollment: enrollment, aeroKey: aeroKey, subscriptions: "true", includeInbound: "true",
			origin: "https://plano.example", wantError: "PLANO_AEROALERTS_KEY_FILE"},
		{name: "live with inbound subscriptions", enrollment: enrollment, aeroKey: aeroKey, subscriptions: "true", includeInbound: "true",
			origin: "https://plano.example", withAeroCallback: true},
		{name: "demo skips inbound subscription settings", demo: "true", includeInbound: invalidFlag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateStartupEnvironment(t)
			t.Setenv("PLANO_DEMO", tc.demo)
			t.Setenv("PLANO_ALLOW_OPEN_ENROLLMENT", tc.allowOpen)
			t.Setenv("PLANO_REQUIRE_ENROLLMENT", tc.requireEnrollment)
			t.Setenv("PLANO_ENROLLMENT_CODE", tc.enrollment)
			t.Setenv("PLANO_AEROAPI_KEY", tc.aeroKey)
			t.Setenv("PLANO_TAF", tc.taf)
			t.Setenv("PLANO_PUBLIC_URL", tc.origin)
			t.Setenv("PLANO_AERO_SUBSCRIPTIONS", tc.subscriptions)
			t.Setenv("PLANO_AERO_SUBSCRIPTIONS_INBOUND", tc.includeInbound)
			if tc.withAeroCallback {
				path := filepath.Join(t.TempDir(), "callback-key")
				if err := os.WriteFile(path, []byte(aeroCallbackKey), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PLANO_AEROALERTS_KEY_FILE", path)
			}
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
			for _, value := range []string{tc.enrollment, tc.aeroKey, aeroCallbackKey, invalidFlag} {
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

func TestCheckConfigAcceptsFAARegistryArchiveAndLegacyMaster(t *testing.T) {
	const master = "N-NUMBER,MFR MDL CODE,YEAR MFR,STATUS CODE,MODE S CODE HEX\n12AA,0123456,2010,V,A00001\n"
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, entry := range []struct{ name, data string }{
		{"MASTER.txt", master},
		{"ACFTREF.txt", "CODE,MFR,MODEL\n0123456,FIXTURE AIRCRAFT,100\n"},
	} {
		file, err := writer.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		data  []byte
		model string
	}{
		{"archive without filename extension", archive.Bytes(), "FIXTURE AIRCRAFT 100"},
		{"legacy master", []byte(master), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateStartupEnvironment(t)
			t.Setenv("PLANO_DEMO", "false")
			t.Setenv("PLANO_ENROLLMENT_CODE", "fixture-enrollment-code")
			t.Setenv("PLANO_AEROAPI_KEY", "fixture-aeroapi-key")
			t.Setenv("PLANO_ADDR", "127.0.0.1:0")
			path := filepath.Join(t.TempDir(), "registry")
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PLANO_FAA_REGISTRY_FILE", path)
			if err := run(slog.New(slog.DiscardHandler), true); err != nil {
				t.Fatalf("check-config rejected registry: %v", err)
			}
			registry, err := loadFAARegistryFile(path)
			if err != nil || registry.ModeSHex("N12AA") != "a00001" || registry.Model("N12AA") != tc.model {
				t.Fatalf("registry mount did not load the expected aircraft, error = %v", err)
			}
		})
	}
}

func TestCheckConfigFlightObservationKey(t *testing.T) {
	key := strings.Repeat("fixture-key", 4)
	for _, tc := range []struct {
		name, demo, key, otherKeyEnv, wantError string
		emptyFile                               bool
	}{
		{name: "no live key", demo: "false"},
		{name: "valid live key", demo: "false", key: key + "\n"},
		{name: "short live key", demo: "false", key: "short-fixture-key", wantError: "requires at least 32 bytes"},
		{name: "empty live key", demo: "false", emptyFile: true, wantError: "empty secret file for " + flightObservationKeyFile},
		{name: "whitespace live key", demo: "false", key: " \t\n", wantError: "empty secret file for " + flightObservationKeyFile},
		{name: "live key directory", demo: "false", wantError: "must name a readable key file"},
		{name: "shared AeroAPI callback key", demo: "false", key: key, otherKeyEnv: "PLANO_AEROALERTS_KEY_FILE", wantError: "must use a distinct key"},
		{name: "shared advisory ingress key", demo: "false", key: key, otherKeyEnv: "PLANO_ADVISORY_INGRESS_KEY_FILE", wantError: "must use a distinct key"},
		{name: "demo skips live key file", demo: "true", key: "short-fixture-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateStartupEnvironment(t)
			t.Setenv("PLANO_DEMO", tc.demo)
			t.Setenv("PLANO_ALLOW_OPEN_ENROLLMENT", "false")
			t.Setenv("PLANO_ENROLLMENT_CODE", "fixture-enrollment-code")
			t.Setenv("PLANO_AEROAPI_KEY", "fixture-aeroapi-key")
			t.Setenv("PLANO_ADDR", "127.0.0.1:0")
			path := t.TempDir()
			if tc.key != "" || tc.emptyFile {
				path = filepath.Join(path, "flight-observation-key")
				if err := os.WriteFile(path, []byte(tc.key), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.key != "" || tc.wantError != "" {
				t.Setenv(flightObservationKeyFile, path)
			}
			var otherPath string
			if tc.otherKeyEnv != "" {
				otherPath = filepath.Join(t.TempDir(), "other-callback-key")
				if err := os.WriteFile(otherPath, []byte(tc.key+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv(tc.otherKeyEnv, otherPath)
			}
			err := run(slog.New(slog.DiscardHandler), true)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("check-config failed: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), flightObservationKeyFile) || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected flight observation key configuration error, got %v", err)
			}
			if strings.Contains(err.Error(), path) || otherPath != "" && strings.Contains(err.Error(), otherPath) || tc.key != "" && strings.Contains(err.Error(), tc.key) {
				t.Fatal("configuration diagnostic exposed the key or its path")
			}
		})
	}
}

func TestCheckConfigSWIMPositionKey(t *testing.T) {
	isolateStartupEnvironment(t)
	t.Setenv("PLANO_DEMO", "false")
	t.Setenv("PLANO_ALLOW_OPEN_ENROLLMENT", "false")
	t.Setenv("PLANO_ENROLLMENT_CODE", "fixture-enrollment-code")
	t.Setenv("PLANO_AEROAPI_KEY", "fixture-api-key")
	t.Setenv("PLANO_ADDR", "127.0.0.1:0")
	path := filepath.Join(t.TempDir(), "swim-key")
	if err := os.WriteFile(path, []byte(strings.Repeat("fixture-swim", 4)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(swimPositionKeyFile, path)
	if err := run(slog.New(slog.DiscardHandler), true); err != nil {
		t.Fatal(err)
	}
	t.Setenv(eurocontrolPositionKeyFile, path)
	if err := run(slog.New(slog.DiscardHandler), true); err == nil || !strings.Contains(err.Error(), eurocontrolPositionKeyFile+" must use a distinct key") {
		t.Fatalf("shared position callback key, got %v", err)
	}
	t.Setenv(eurocontrolPositionKeyFile, "")
	t.Setenv(flightObservationKeyFile, path)
	if err := run(slog.New(slog.DiscardHandler), true); err == nil || !strings.Contains(err.Error(), swimPositionKeyFile+" must use a distinct key") {
		t.Fatalf("shared aviation callback key, got %v", err)
	}
}

func isolateStartupEnvironment(t *testing.T) {
	t.Helper()
	// Preflight must only see fixture settings, including on an operator host.
	for _, name := range []string{
		"PLANO_PUBLIC_URL", "PLANO_REQUIRE_ENROLLMENT", "PLANO_ENROLLMENT_CODE_FILE", "PLANO_BOOKING_KEY_FILE", "PLANO_AIRPORT_TRANSFERS_FILE",
		"PLANO_AEROALERTS_KEY_FILE", "PLANO_ADVISORY_INGRESS_KEY_FILE", flightObservationKeyFile, swimPositionKeyFile, eurocontrolPositionKeyFile, "PLANO_AEROAPI_KEY_FILE",
		"PLANO_METAR", "PLANO_TAF", "PLANO_AERO_SUBSCRIPTIONS", "PLANO_AERO_SUBSCRIPTIONS_INBOUND", "PLANO_AERO_SUBSCRIPTIONS_MAX", "PLANO_AERO_SUBSCRIPTIONS_INTERVAL",
		"PLANO_TRIPIT_SYNC_KEY_FILE", "PLANO_RESERVATION_GATEWAY_URL",
		"PLANO_FAA_NAS", "PLANO_SWIM_URL", "PLANO_EUROCONTROL_URL", "PLANO_ADSB_URL", "PLANO_FAA_REGISTRY_FILE",
		"PLANO_APNS_TEAM_ID", "PLANO_APNS_KEY_ID", "PLANO_APNS_BUNDLE_ID", "PLANO_APNS_PRIVATE_KEY_FILE", "PLANO_APNS_SANDBOX",
		"PLANO_FCM_PROJECT_ID", "PLANO_FCM_SERVICE_ACCOUNT_FILE", "PLANO_FCM_BANDWIDTH_CONSTRAINED",
		"PLANO_REFRESH_INTERVAL", "PLANO_DELAY_MODEL_FILE", "PLANO_EARLY_DELAY_MODEL_FILE", "PLANO_HISTORICAL_RISK_FILE", "PLANO_COLLECT_DELAY_SAMPLES",
	} {
		t.Setenv(name, "")
	}
}

func TestCheckConfigEarlyDelayModel(t *testing.T) {
	for _, tc := range []struct {
		name, demo, wantError string
		lead                  time.Duration
	}{
		{name: "invalid artifact", demo: "false", wantError: "PLANO_EARLY_DELAY_MODEL_FILE"},
		{name: "standard artifact", demo: "false", lead: 2 * time.Hour, wantError: "above 120 minutes"},
		{name: "early artifact", demo: "false", lead: 3 * time.Hour},
		{name: "demo skips invalid artifact", demo: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateStartupEnvironment(t)
			t.Setenv("PLANO_DEMO", tc.demo)
			t.Setenv("PLANO_ALLOW_OPEN_ENROLLMENT", "false")
			t.Setenv("PLANO_ENROLLMENT_CODE", "fixture-enrollment-code")
			t.Setenv("PLANO_AEROAPI_KEY", "fixture-aeroapi-key")
			t.Setenv("PLANO_ADDR", "127.0.0.1:0")
			path := filepath.Join(t.TempDir(), "early-delay-model.json")
			var data bytes.Buffer
			if tc.lead == 0 {
				data.WriteString(`{"schemaVersion":0}`)
			} else {
				examples := make([]predict.Example, 40)
				for i := range examples {
					scheduled := time.Date(2026, time.January, 1+i, 12, 0, 0, 0, time.UTC)
					examples[i] = predict.Example{
						FlightID: fmt.Sprintf("fixture-%d", i), ObservedAt: scheduled.Add(-tc.lead),
						ScheduledDeparture: scheduled, DepartedAt: scheduled.Add(15 * time.Minute),
						Features: predict.Features{AirlineDelayMinutes: 10},
					}
				}
				model, _, err := predict.Fit(examples, predict.Options{HoldoutFraction: 0.2, Ridge: 0.1})
				if err != nil {
					t.Fatal(err)
				}
				if err := model.Save(&data); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PLANO_EARLY_DELAY_MODEL_FILE", path)
			err := run(slog.New(slog.DiscardHandler), true)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("check-config error = %v, want diagnostic containing %q", err, tc.wantError)
			}
		})
	}
}

func TestCheckConfigRejectsInvalidHistoricalRisk(t *testing.T) {
	isolateStartupEnvironment(t)
	t.Setenv("PLANO_DEMO", "true")
	t.Setenv("PLANO_ENROLLMENT_CODE", "")
	t.Setenv("PLANO_ADDR", "127.0.0.1:0")
	path := filepath.Join(t.TempDir(), "historical-risk.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLANO_HISTORICAL_RISK_FILE", path)
	if err := run(slog.New(slog.DiscardHandler), true); err == nil || !strings.Contains(err.Error(), "PLANO_HISTORICAL_RISK_FILE") {
		t.Fatalf("check-config accepted invalid historical risk artifact: %v", err)
	}
}
