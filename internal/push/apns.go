package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"
)

type APNsConfig struct {
	TeamID        string
	KeyID         string
	BundleID      string
	PrivateKeyPEM []byte
	Sandbox       bool
}

type APNs struct {
	client   *http.Client
	endpoint string
	teamID   string
	keyID    string
	bundleID string
	key      *ecdsa.PrivateKey

	mu     sync.Mutex
	token  string
	issued time.Time
}

var appleKeyIDPattern = regexp.MustCompile(`^[A-Z0-9]{10}$`)
var bundleIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+$`)

func NewAPNs(config APNsConfig) (*APNs, error) {
	if !appleKeyIDPattern.MatchString(config.TeamID) || !appleKeyIDPattern.MatchString(config.KeyID) || !bundleIDPattern.MatchString(config.BundleID) {
		return nil, errors.New("APNs requires valid team, key and bundle IDs")
	}
	block, _ := pem.Decode(config.PrivateKeyPEM)
	if block == nil {
		return nil, errors.New("APNs private key is not PEM")
	}
	var key *ecdsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid APNs PKCS#8 private key")
		}
		key, _ = parsed.(*ecdsa.PrivateKey)
	case "EC PRIVATE KEY":
		var err error
		key, err = x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid APNs EC private key")
		}
	}
	if key == nil || key.Curve != elliptic.P256() {
		return nil, errors.New("APNs requires a P-256 private key")
	}
	endpoint := "https://api.push.apple.com"
	if config.Sandbox {
		endpoint = "https://api.sandbox.push.apple.com"
	}
	return &APNs{
		client:   makeClient(true),
		endpoint: endpoint,
		teamID:   config.TeamID,
		keyID:    config.KeyID,
		bundleID: config.BundleID,
		key:      key,
	}, nil
}

func (a *APNs) providerToken(now time.Time) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && now.Sub(a.issued) < 45*time.Minute {
		return a.token, nil
	}
	unsigned := unsignedJWT(
		map[string]string{"alg": "ES256", "kid": a.keyID},
		map[string]any{"iss": a.teamID, "iat": now.Unix()},
	)
	digest := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, a.key, digest[:])
	if err != nil {
		return "", &DeliveryError{Provider: "APNs", Code: "SigningFailed"}
	}
	var signature [64]byte
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	a.token = signedJWT(unsigned, signature[:])
	a.issued = now
	return a.token, nil
}

func (a *APNs) refreshExpired(token string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != token {
		return true
	}
	// A newly issued token being deemed stale indicates a clock or key issue.
	// Do not churn provider tokens when several requests fail together.
	if now.Sub(a.issued) < 20*time.Minute {
		return false
	}
	a.token = ""
	return true
}

type apnsAlert struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type apnsAPS struct {
	Alert          *apnsAlert `json:"alert,omitempty"`
	Sound          string     `json:"sound,omitempty"`
	Timestamp      int64      `json:"timestamp,omitempty"`
	Event          Kind       `json:"event,omitempty"`
	ContentState   *LiveState `json:"content-state,omitempty"`
	AttributesType string     `json:"attributes-type,omitempty"`
	Attributes     *struct {
		FlightID string `json:"flightId"`
	} `json:"attributes,omitempty"`
	InputPushToken int   `json:"input-push-token,omitempty"`
	StaleDate      int64 `json:"stale-date,omitempty"`
	DismissalDate  int64 `json:"dismissal-date,omitempty"`
}

func (a *APNs) request(m Message) ([]byte, http.Header, error) {
	aps := apnsAPS{}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("apns-expiration", strconv.FormatInt(m.ExpiresAt.Unix(), 10))
	headers.Set("apns-priority", "10")
	if m.CollapseID != "" && (m.Kind == Alert || m.Kind == LiveUpdate) {
		headers.Set("apns-collapse-id", m.CollapseID)
	}
	payload := struct {
		APS                 *apnsAPS `json:"aps"`
		FlightID            string   `json:"flightId,omitempty"`
		ConnectionInboundID string   `json:"connectionInboundId,omitempty"`
	}{APS: &aps}

	if m.Kind == Alert {
		headers.Set("apns-push-type", "alert")
		headers.Set("apns-topic", a.bundleID)
		aps.Alert = &apnsAlert{Title: m.Title, Body: m.Body}
		aps.Sound = "default"
		payload.FlightID = m.FlightID
		payload.ConnectionInboundID = m.ConnectionInboundID
	} else {
		headers.Set("apns-push-type", "liveactivity")
		headers.Set("apns-topic", a.bundleID+".push-type.liveactivity")
		aps.Timestamp = m.Timestamp.Unix()
		aps.Event = m.Kind
		aps.ContentState = m.State
		if !m.StaleAt.IsZero() && m.Kind != LiveEnd {
			aps.StaleDate = m.StaleAt.Unix()
		}
		switch m.Kind {
		case LiveStart:
			aps.AttributesType = "PlanoActivityAttributes"
			aps.Attributes = &struct {
				FlightID string `json:"flightId"`
			}{FlightID: m.FlightID}
			title, body := m.Title, m.Body
			if title == "" {
				title = "Flight tracking started"
			}
			if body == "" {
				body = "Plano is tracking your flight."
			}
			aps.Alert = &apnsAlert{Title: title, Body: body}
			if m.RequestUpdateToken {
				aps.InputPushToken = 1
			}
		case LiveUpdate:
			headers.Set("apns-priority", "5")
		case LiveEnd:
			if !m.DismissAt.IsZero() {
				aps.DismissalDate = m.DismissAt.Unix()
			}
		}
	}

	body, err := encodePayload(payload)
	return body, headers, err
}

func (a *APNs) Send(ctx context.Context, deviceToken string, message Message) error {
	if len(deviceToken) == 0 || len(deviceToken) > 512 || len(deviceToken)%2 != 0 {
		return &DeliveryError{Provider: "APNs", Code: "MalformedDeviceToken", InvalidToken: true}
	}
	var tokenBytes [256]byte
	if _, err := hex.Decode(tokenBytes[:], []byte(deviceToken)); err != nil {
		return &DeliveryError{Provider: "APNs", Code: "MalformedDeviceToken", InvalidToken: true}
	}
	now := time.Now()
	m, err := message.prepare(now)
	if err != nil {
		return err
	}
	payload, headers, err := a.request(m)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := a.providerToken(time.Now())
		if err != nil {
			return err
		}
		headers.Set("Authorization", "bearer "+token)
		response, err := post(ctx, a.client, "APNs", a.endpoint+"/3/device/"+deviceToken, headers, payload)
		if err != nil {
			return err
		}
		if response.status == http.StatusOK {
			return nil
		}
		rejection := apnsRejection(response)
		if rejection.Code != "ExpiredProviderToken" || attempt != 0 || !a.refreshExpired(token, time.Now()) {
			return rejection
		}
	}
	panic("unreachable APNs retry loop")
}

func apnsRejection(response providerResponse) *DeliveryError {
	var result struct {
		Reason    string `json:"reason"`
		Timestamp int64  `json:"timestamp"`
	}
	_ = json.Unmarshal(response.body, &result)
	status := response.status
	rejection := &DeliveryError{
		Provider:     "APNs",
		Code:         safeReason(result.Reason),
		Status:       status,
		InvalidToken: status == http.StatusGone,
		Retryable:    status >= 500 || status == http.StatusTooManyRequests,
		RetryAfter:   response.retryAfter,
	}
	if rejection.InvalidToken && result.Timestamp > 0 {
		rejection.TokenInvalidatedAt = time.UnixMilli(result.Timestamp)
	}
	if status >= 500 {
		rejection.RetryAfter = max(rejection.RetryAfter, 15*time.Minute)
	}
	if result.Reason == "TooManyProviderTokenUpdates" {
		rejection.RetryAfter = max(rejection.RetryAfter, 20*time.Minute)
	}
	return rejection
}
