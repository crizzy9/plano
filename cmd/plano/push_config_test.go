package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPushConfigRequiresCompleteCredentialGroups(t *testing.T) {
	clearPushConfigEnvironment(t)
	const rejectedValue = "private-fixture-marker"
	for _, tt := range []struct {
		name string
		set  map[string]string
		want string
	}{
		{name: "disabled"},
		{name: "disabled with explicit defaults", set: map[string]string{"PLANO_APNS_SANDBOX": "false", "PLANO_FCM_BANDWIDTH_CONSTRAINED": "false"}},
		{name: "APNs team only", set: map[string]string{"PLANO_APNS_TEAM_ID": rejectedValue}, want: "APNs requires PLANO_APNS_TEAM_ID"},
		{name: "APNs key file only", set: map[string]string{"PLANO_APNS_PRIVATE_KEY_FILE": rejectedValue}, want: "APNs requires PLANO_APNS_TEAM_ID"},
		{name: "APNs sandbox only", set: map[string]string{"PLANO_APNS_SANDBOX": "true"}, want: "APNs requires PLANO_APNS_TEAM_ID"},
		{name: "invalid APNs flag", set: map[string]string{"PLANO_APNS_SANDBOX": rejectedValue}, want: "PLANO_APNS_SANDBOX must be a boolean"},
		{name: "FCM project only", set: map[string]string{"PLANO_FCM_PROJECT_ID": rejectedValue}, want: "FCM requires PLANO_FCM_SERVICE_ACCOUNT_FILE"},
		{name: "FCM missing account file", set: map[string]string{"PLANO_FCM_SERVICE_ACCOUNT_FILE": rejectedValue}, want: "cannot read PLANO_FCM_SERVICE_ACCOUNT_FILE"},
		{name: "FCM bandwidth option only", set: map[string]string{"PLANO_FCM_BANDWIDTH_CONSTRAINED": "true"}, want: "FCM requires PLANO_FCM_SERVICE_ACCOUNT_FILE"},
		{name: "invalid FCM flag", set: map[string]string{"PLANO_FCM_BANDWIDTH_CONSTRAINED": rejectedValue}, want: "PLANO_FCM_BANDWIDTH_CONSTRAINED must be a boolean"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for name, value := range tt.set {
				t.Setenv(name, value)
			}
			sender, err := pushConfig("https://plano.example")
			if tt.want == "" {
				if err != nil || sender.APNs != nil || sender.FCM != nil {
					t.Fatal("expected both providers to stay disabled")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), rejectedValue) {
				t.Fatalf("expected a redacted error containing %q", tt.want)
			}
		})
	}
}

func TestPushConfigRejectsEmptyCredentialFiles(t *testing.T) {
	clearPushConfigEnvironment(t)
	path := writePushFixture(t, []byte(" \n"))
	t.Run("APNs", func(t *testing.T) {
		setAPNsFixture(t, path)
		if _, err := pushConfig("https://plano.example"); err == nil || strings.Contains(err.Error(), path) {
			t.Fatal("expected a redacted APNs error")
		}
	})
	t.Run("FCM", func(t *testing.T) {
		t.Setenv("PLANO_FCM_PROJECT_ID", "fixture-project")
		t.Setenv("PLANO_FCM_SERVICE_ACCOUNT_FILE", path)
		if _, err := pushConfig("https://plano.example"); err == nil || strings.Contains(err.Error(), path) {
			t.Fatal("expected a redacted FCM error")
		}
	})
}

func TestPushConfigInitializesBothProvidersOffline(t *testing.T) {
	clearPushConfigEnvironment(t)
	apnsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	apnsDER, err := x509.MarshalECPrivateKey(apnsKey)
	if err != nil {
		t.Fatal(err)
	}
	setAPNsFixture(t, writePushFixture(t, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: apnsDER})))
	t.Setenv("PLANO_APNS_SANDBOX", "true")

	fcmKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fcmPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(fcmKey)})
	account, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "fixture-project",
		"client_email": "fixture@example.invalid", "private_key": string(fcmPEM),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLANO_FCM_SERVICE_ACCOUNT_FILE", writePushFixture(t, account))
	t.Setenv("PLANO_FCM_BANDWIDTH_CONSTRAINED", "true")

	sender, err := pushConfig("https://plano.example")
	if err != nil {
		t.Fatal(err)
	}
	if sender.APNs == nil || sender.FCM == nil {
		t.Fatal("expected both providers to initialize from fixture credentials")
	}
}

func clearPushConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PLANO_APNS_TEAM_ID", "PLANO_APNS_KEY_ID", "PLANO_APNS_BUNDLE_ID", "PLANO_APNS_PRIVATE_KEY_FILE", "PLANO_APNS_SANDBOX",
		"PLANO_FCM_PROJECT_ID", "PLANO_FCM_SERVICE_ACCOUNT_FILE", "PLANO_FCM_BANDWIDTH_CONSTRAINED",
	} {
		t.Setenv(name, "")
	}
}

func setAPNsFixture(t *testing.T, keyPath string) {
	t.Helper()
	t.Setenv("PLANO_APNS_TEAM_ID", "TEAMID1234")
	t.Setenv("PLANO_APNS_KEY_ID", "KEYID12345")
	t.Setenv("PLANO_APNS_BUNDLE_ID", "example.invalid.plano")
	t.Setenv("PLANO_APNS_PRIVATE_KEY_FILE", keyPath)
}

func writePushFixture(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
