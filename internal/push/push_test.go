package push

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	defer request.Body.Close()
	return f(request)
}

func fixtureResponse(status int, body string, headers http.Header) (*http.Response, error) {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: headers}, nil
}

func fixtureAPNs(t *testing.T) *APNs {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewAPNs(APNsConfig{
		TeamID: "A123456789", KeyID: "B123456789", BundleID: "com.example.plano", Sandbox: true,
		PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func fixtureJWT(token string) ([]byte, []byte, []byte, [32]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, nil, [32]byte{}, errors.New("JWT must have three sections")
	}
	var decoded [3][]byte
	for i, part := range parts {
		var err error
		decoded[i], err = base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			return nil, nil, nil, [32]byte{}, err
		}
	}
	return decoded[0], decoded[1], decoded[2], sha256.Sum256([]byte(parts[0] + "." + parts[1])), nil
}

func fixtureAlert(now time.Time) Message {
	return Message{
		Kind: Alert, FlightID: "flight-fixture", Title: "Gate changed", Body: "UA7 · C12",
		CollapseID: "event-fixture", Timestamp: now, ExpiresAt: now.Add(90 * time.Second),
	}
}

func fixtureActivity(kind Kind, now time.Time) Message {
	return Message{
		Kind: kind, FlightID: "flight-fixture", CollapseID: "flight-update", Timestamp: now, ExpiresAt: now.Add(90 * time.Second),
		StaleAt: now.Add(2 * time.Minute), DismissAt: now.Add(4 * time.Minute),
		State: &LiveState{FlightID: "flight-fixture", FlightNumber: "UA7", Status: "airborne", Origin: "SFO", Destination: "JFK",
			DepartureGate: "C12", ArrivalGate: "4", DepartureTerminal: "3", ArrivalTerminal: "7", Seat: "14A",
			DepartureEpoch: now.Add(-10 * time.Minute).Unix(), ArrivalEpoch: now.Add(5 * time.Hour).Unix(), ObservedEpoch: now.Unix()},
	}
}

func TestAPNsWireRequests(t *testing.T) {
	client := fixtureAPNs(t)
	transport := client.client.Transport.(*http.Transport)
	if transport.Protocols.HTTP1() || !transport.Protocols.HTTP2() {
		t.Fatal("APNs transport must use HTTP/2 only")
	}
	const token = "aabbccddeeff00112233445566778899"
	now := time.Now().Truncate(time.Second)
	for _, test := range []struct {
		name, topic, pushType, priority, collapse string
		message                                   Message
		inputToken                                int
	}{
		{"alert", "com.example.plano", "alert", "10", "event-fixture", fixtureAlert(now), 0},
		{"start", "com.example.plano.push-type.liveactivity", "liveactivity", "10", "", fixtureActivity(LiveStart, now), 0},
		{"start requests update token", "com.example.plano.push-type.liveactivity", "liveactivity", "10", "", fixtureActivity(LiveStart, now), 1},
		{"update", "com.example.plano.push-type.liveactivity", "liveactivity", "5", "flight-update", fixtureActivity(LiveUpdate, now), 0},
		{"end", "com.example.plano.push-type.liveactivity", "liveactivity", "10", "", fixtureActivity(LiveEnd, now), 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client.client = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "POST" || r.URL.String() != "https://api.sandbox.push.apple.com/3/device/"+token {
					t.Errorf("unexpected APNs destination: %s %s", r.Method, r.URL)
				}
				wantHeaders := map[string]string{
					"Content-Type": "application/json", "apns-topic": test.topic, "apns-push-type": test.pushType,
					"apns-priority": test.priority, "apns-collapse-id": test.collapse,
					"apns-expiration": strconv.FormatInt(test.message.ExpiresAt.Unix(), 10),
				}
				for name, want := range wantHeaders {
					if got := r.Header.Get(name); got != want {
						t.Errorf("header %s = %q, want %q", name, got, want)
					}
				}
				auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "bearer ")
				if !ok {
					t.Error("missing APNs provider token")
				}
				header, claims, signature, digest, err := fixtureJWT(auth)
				if err != nil || string(header) != `{"alg":"ES256","kid":"B123456789"}` || len(signature) != 64 ||
					!ecdsa.Verify(&client.key.PublicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
					t.Error("APNs ES256 provider token is invalid")
				}
				var provider struct {
					Issuer string `json:"iss"`
					Issued int64  `json:"iat"`
				}
				if err := json.Unmarshal(claims, &provider); err != nil || provider.Issuer != "A123456789" || provider.Issued < now.Unix() || provider.Issued > time.Now().Unix() {
					t.Errorf("invalid APNs issuer or issuance time: %+v", provider)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || len(body) > maxPayloadBytes {
					t.Fatalf("reading APNs body: bytes=%d, error=%v", len(body), err)
				}
				var wire struct {
					APS struct {
						Event      string            `json:"event"`
						Time       int64             `json:"timestamp"`
						Stale      int64             `json:"stale-date"`
						Dismiss    int64             `json:"dismissal-date"`
						InputToken int               `json:"input-push-token"`
						Type       string            `json:"attributes-type"`
						Attributes map[string]string `json:"attributes"`
						Alert      map[string]string `json:"alert"`
						State      *LiveState        `json:"content-state"`
					} `json:"aps"`
					FlightID string `json:"flightId"`
				}
				if err := json.Unmarshal(body, &wire); err != nil {
					t.Fatal(err)
				}
				aps := wire.APS
				if test.message.Kind == Alert {
					if wire.FlightID != test.message.FlightID || aps.Alert["title"] != test.message.Title || aps.Alert["body"] != test.message.Body || aps.Event != "" {
						t.Errorf("invalid APNs alert: %s", body)
					}
					return fixtureResponse(200, "", nil)
				}
				if aps.State == nil || *aps.State != *test.message.State || aps.Event != string(test.message.Kind) || aps.Time != now.Unix() {
					t.Errorf("invalid APNs activity: %s", body)
				}
				switch test.message.Kind {
				case LiveStart:
					if aps.Type != "PlanoActivityAttributes" || aps.Attributes["flightId"] != test.message.FlightID || aps.Alert["title"] == "" || aps.Alert["body"] == "" || aps.InputToken != test.inputToken || aps.Stale != test.message.StaleAt.Unix() {
						t.Errorf("invalid APNs start: %s", body)
					}
				case LiveUpdate:
					if aps.Type != "" || aps.Alert != nil || aps.Stale != test.message.StaleAt.Unix() || aps.Dismiss != 0 {
						t.Errorf("invalid APNs update: %s", body)
					}
				case LiveEnd:
					if aps.Stale != 0 || aps.Dismiss != test.message.DismissAt.Unix() {
						t.Errorf("invalid APNs end: %s", body)
					}
				}
				return fixtureResponse(200, "", nil)
			})}
			message := test.message
			message.RequestUpdateToken = test.inputToken != 0
			if err := client.Send(t.Context(), token, message); err != nil || calls != 1 {
				t.Fatalf("APNs send: calls=%d, error=%v", calls, err)
			}
		})
	}
}

func TestAPNsRefreshesRejectedCachedProviderTokenOnce(t *testing.T) {
	client := fixtureAPNs(t)
	now := time.Now()
	old, err := client.providerToken(now.Add(-30 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client.client = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		auth := r.Header.Get("Authorization")
		if calls == 1 {
			if auth != "bearer "+old {
				t.Error("first attempt did not reuse cached APNs token")
			}
			return fixtureResponse(403, `{"reason":"ExpiredProviderToken"}`, nil)
		}
		if auth == "bearer "+old {
			t.Error("expired APNs provider token was not refreshed")
		}
		return fixtureResponse(200, "", nil)
	})}
	if err := client.Send(t.Context(), "abcdef", fixtureAlert(now)); err != nil || calls != 2 {
		t.Fatalf("APNs refresh: calls=%d, error=%v", calls, err)
	}
}

func TestFCMOAuthAndAndroidDataRequests(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	const accountEmail = "plano@fixture-project.iam.gserviceaccount.com"
	account, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "fixture-project", "client_email": accountEmail,
		"private_key_id": "fixture-key", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})),
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewFCM(FCMConfig{ServiceAccountJSON: account, BandwidthConstrained: true})
	if err != nil {
		t.Fatal(err)
	}
	const device = "fixture:android-registration"
	now := time.Now()
	messages := []Message{fixtureAlert(now), fixtureActivity(LiveStart, now), fixtureActivity(LiveUpdate, now), fixtureActivity(LiveEnd, now)}
	var exchanges, sends int
	client.client = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		switch r.URL.String() {
		case "https://oauth2.googleapis.com/token":
			exchanges++
			form, err := url.ParseQuery(string(body))
			if err != nil || r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Error("invalid Google OAuth exchange request")
			}
			header, claims, signature, digest, err := fixtureJWT(form.Get("assertion"))
			if err != nil || string(header) != `{"alg":"RS256","kid":"fixture-key","typ":"JWT"}` || rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
				t.Error("invalid Google OAuth assertion signature")
			}
			var jwt struct {
				Issuer   string `json:"iss"`
				Scope    string `json:"scope"`
				Audience string `json:"aud"`
				Issued   int64  `json:"iat"`
				Expires  int64  `json:"exp"`
			}
			if err := json.Unmarshal(claims, &jwt); err != nil || jwt.Issuer != accountEmail || jwt.Scope != "https://www.googleapis.com/auth/firebase.messaging" || jwt.Audience != "https://oauth2.googleapis.com/token" || jwt.Issued < now.Unix() || jwt.Expires-jwt.Issued != 3600 {
				t.Errorf("invalid Google OAuth claims: %+v", jwt)
			}
			return fixtureResponse(200, fmt.Sprintf(`{"access_token":"fixture-access-%d","token_type":"Bearer","expires_in":3600}`, exchanges), nil)
		case "https://fcm.googleapis.com/v1/projects/fixture-project/messages:send":
			if sends >= len(messages) {
				return nil, errors.New("unexpected FCM retry")
			}
			message := messages[sends]
			sends++
			if r.Method != "POST" || r.Header.Get("Authorization") != fmt.Sprintf("Bearer fixture-access-%d", exchanges) || r.Header.Get("Content-Type") != "application/json" {
				t.Error("invalid FCM send headers")
			}
			var wire struct {
				Message struct {
					Token        string            `json:"token"`
					Data         map[string]string `json:"data"`
					Notification json.RawMessage   `json:"notification"`
					Android      struct {
						Priority  string          `json:"priority"`
						TTL       string          `json:"ttl"`
						Collapse  string          `json:"collapse_key"`
						Bandwidth bool            `json:"bandwidth_constrained_ok"`
						UI        json.RawMessage `json:"notification"`
					} `json:"android"`
				} `json:"message"`
			}
			if err := json.Unmarshal(body, &wire); err != nil {
				return nil, err
			}
			got := wire.Message
			ttl, err := time.ParseDuration(got.Android.TTL)
			if got.Token != device || got.Android.Priority != "HIGH" || !got.Android.Bandwidth || err != nil || ttl <= 0 || ttl > 90*time.Second || got.Notification != nil || got.Android.UI != nil || got.Data["flightId"] != message.FlightID {
				t.Errorf("invalid FCM data request: %s", body)
			}
			if message.Kind == Alert {
				if got.Data["type"] != "alert" || got.Data["title"] != message.Title || got.Data["body"] != message.Body || got.Data["eventId"] != message.CollapseID {
					t.Errorf("invalid FCM alert: %s", body)
				}
			} else if got.Data["type"] != "live" || got.Data["event"] != string(message.Kind) || got.Data["departureGate"] != "C12" || got.Data["seat"] != "14A" || got.Data["observedEpoch"] != strconv.FormatInt(message.State.ObservedEpoch, 10) {
				t.Errorf("invalid FCM activity: %s", body)
			}
			wantCollapse := ""
			if message.Kind == LiveUpdate {
				wantCollapse = message.CollapseID
			}
			if got.Android.Collapse != wantCollapse {
				t.Errorf("FCM collapse key = %q, want %q", got.Android.Collapse, wantCollapse)
			}
			if sends == 2 {
				return fixtureResponse(401, `{"error":{"status":"UNAUTHENTICATED"}}`, nil)
			}
			return fixtureResponse(200, `{"name":"projects/fixture-project/messages/fixture"}`, nil)
		default:
			return nil, fmt.Errorf("unexpected fixture URL: %s", r.URL.Host)
		}
	})}
	for i, message := range messages {
		err := client.Send(t.Context(), device, message)
		if i == 1 {
			var rejection *DeliveryError
			if !errors.As(err, &rejection) || rejection.InvalidToken || rejection.Retryable || rejection.Status != 401 {
				t.Fatalf("FCM 401 classification: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if sends != 4 || exchanges != 2 {
		t.Fatalf("OAuth cache / invalidation: sends=%d, exchanges=%d", sends, exchanges)
	}
}

func TestProviderRejectionsAndRetryAfter(t *testing.T) {
	const fcmType = "type.googleapis.com/google.firebase.fcm.v1.FcmError"
	for _, test := range []struct {
		name    string
		got     *DeliveryError
		code    string
		invalid bool
		wait    time.Duration
		retry   bool
	}{
		{"APNs unregister", apnsRejection(providerResponse{status: 410, body: []byte(`{"reason":"Unregistered","timestamp":1800000000123}`)}), "Unregistered", true, 0, false},
		{"APNs uncertain environment", apnsRejection(providerResponse{status: 400, body: []byte(`{"reason":"BadDeviceToken"}`)}), "BadDeviceToken", false, 0, false},
		{"APNs unavailable", apnsRejection(providerResponse{status: 503, body: []byte(`{"reason":"ServiceUnavailable"}`)}), "ServiceUnavailable", false, 15 * time.Minute, true},
		{"APNs throttled", apnsRejection(providerResponse{status: 429, body: []byte(`{"reason":"TooManyRequests"}`), retryAfter: 20 * time.Second}), "TooManyRequests", false, 20 * time.Second, true},
		{"FCM unregister", fcmRejection(providerResponse{status: 404, body: []byte(`{"error":{"status":"NOT_FOUND","details":[{"@type":"` + fcmType + `","errorCode":"UNREGISTERED"}]}}`)}), "UNREGISTERED", true, 0, false},
		{"FCM unknown endpoint", fcmRejection(providerResponse{status: 404, body: []byte(`{"error":{"status":"NOT_FOUND"}}`)}), "NOT_FOUND", false, 0, false},
		{"FCM quota", fcmRejection(providerResponse{status: 429, body: []byte(`{"error":{"status":"RESOURCE_EXHAUSTED"}}`), retryAfter: 15 * time.Second}), "RESOURCE_EXHAUSTED", false, time.Minute, true},
		{"FCM unavailable", fcmRejection(providerResponse{status: 503, body: []byte(`{"error":{"status":"UNAVAILABLE"}}`), retryAfter: 45 * time.Second}), "UNAVAILABLE", false, 45 * time.Second, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := test.got
			if got.Code != test.code || got.InvalidToken != test.invalid || got.Retryable != test.retry || got.RetryAfter != test.wait {
				t.Fatalf("classification = %+v", *got)
			}
			if test.name == "APNs unregister" && got.TokenInvalidatedAt.UnixMilli() != 1800000000123 {
				t.Errorf("unexpected invalidation timestamp: %v", got.TokenInvalidatedAt)
			}
		})
	}
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	if got := retryAfter("30", now); got != 30*time.Second {
		t.Errorf("Retry-After seconds = %v", got)
	}
	if got := retryAfter(now.Add(time.Minute).Format(http.TimeFormat), now); got != time.Minute {
		t.Errorf("Retry-After HTTP date = %v", got)
	}
}

func TestOversizedPayloadsAreRejectedBeforeProviderCalls(t *testing.T) {
	client := fixtureAPNs(t)
	transport := &http.Client{Transport: fixtureTransport(func(*http.Request) (*http.Response, error) {
		t.Error("oversized push attempted a provider request")
		return nil, errors.New("unexpected provider request")
	})}
	client.client = transport
	fcm := &FCM{client: transport}
	message := fixtureAlert(time.Now())
	message.Body = strings.Repeat("é", maxPayloadBytes/2)
	for _, err := range []error{client.Send(t.Context(), "aabbccdd", message), fcm.Send(t.Context(), "fixture-registration", message)} {
		if err == nil || !strings.Contains(err.Error(), "2048-byte limit") {
			t.Errorf("expected UTF-8 size rejection, got %v", err)
		}
	}
}
