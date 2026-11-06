package push

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	googleOAuthEndpoint = "https://oauth2.googleapis.com/token"
	fcmScope            = "https://www.googleapis.com/auth/firebase.messaging"
	maxFCMTTL           = 28 * 24 * time.Hour
)

type FCMConfig struct {
	ProjectID          string
	ServiceAccountJSON []byte

	// Enable only after verifying constrained-mode behavior on an Android device.
	BandwidthConstrained bool
}

type FCM struct {
	client               *http.Client
	endpoint             string
	tokenEndpoint        string
	email                string
	keyID                string
	key                  *rsa.PrivateKey
	bandwidthConstrained bool

	mu      sync.Mutex
	token   string
	expires time.Time
	refresh *tokenRefresh
}

type tokenRefresh struct {
	done  chan struct{}
	token string
	err   error
}

var projectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

func NewFCM(config FCMConfig) (*FCM, error) {
	var account struct {
		Type       string `json:"type"`
		ProjectID  string `json:"project_id"`
		Email      string `json:"client_email"`
		KeyID      string `json:"private_key_id"`
		PrivateKey string `json:"private_key"`
	}
	if json.Unmarshal(config.ServiceAccountJSON, &account) != nil || account.Type != "service_account" {
		return nil, errors.New("FCM requires a Google service account JSON key")
	}
	project := config.ProjectID
	if project == "" {
		project = account.ProjectID
	}
	if !projectIDPattern.MatchString(project) {
		return nil, errors.New("FCM requires a valid Firebase project ID")
	}
	email, err := mail.ParseAddress(account.Email)
	if err != nil || email.Address != account.Email {
		return nil, errors.New("FCM service account requires a valid email address")
	}
	key, err := parseRSAKey([]byte(account.PrivateKey))
	if err != nil {
		return nil, err
	}
	return &FCM{
		client:               makeClient(false),
		endpoint:             "https://fcm.googleapis.com/v1/projects/" + project + "/messages:send",
		tokenEndpoint:        googleOAuthEndpoint,
		email:                account.Email,
		keyID:                account.KeyID,
		key:                  key,
		bandwidthConstrained: config.BandwidthConstrained,
	}, nil
}

func parseRSAKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("FCM service account private key is not PEM")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid FCM PKCS#8 private key")
		}
		key, _ = parsed.(*rsa.PrivateKey)
	case "RSA PRIVATE KEY":
		var err error
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid FCM RSA private key")
		}
	}
	if key == nil || key.N.BitLen() < 2048 {
		return nil, errors.New("FCM requires an RSA private key of at least 2048 bits")
	}
	return key, nil
}

func (f *FCM) accessToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	if f.token != "" && time.Now().Add(time.Minute).Before(f.expires) {
		token := f.token
		f.mu.Unlock()
		return token, nil
	}
	refresh := f.refresh
	if refresh == nil {
		refresh = &tokenRefresh{done: make(chan struct{})}
		f.refresh = refresh
		go f.refreshToken(refresh)
	}
	f.mu.Unlock()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-refresh.done:
		return refresh.token, refresh.err
	}
}

func (f *FCM) refreshToken(refresh *tokenRefresh) {
	// The refresh is shared by concurrent senders. An individual sender can
	// cancel independently, while the HTTP client's deadline bounds the refresh.
	token, expires, err := f.exchangeToken(context.Background())
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		f.token = token
		f.expires = expires
	}
	refresh.token = token
	refresh.err = err
	f.refresh = nil
	close(refresh.done)
}

func (f *FCM) exchangeToken(ctx context.Context) (string, time.Time, error) {
	now := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if f.keyID != "" {
		header["kid"] = f.keyID
	}
	unsigned := unsignedJWT(header, map[string]any{
		"iss":   f.email,
		"scope": fcmScope,
		"aud":   googleOAuthEndpoint,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", time.Time{}, &DeliveryError{Provider: "FCM OAuth", Code: "SigningFailed"}
	}
	body := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signedJWT(unsigned, signature)},
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := post(ctx, f.client, "FCM OAuth", f.tokenEndpoint, headers, []byte(body.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	decoded := json.Unmarshal(response.body, &result) == nil
	if response.status != http.StatusOK {
		return "", time.Time{}, fcmFailure("FCM OAuth", safeReason(result.Error), response)
	}
	if !decoded || !strings.EqualFold(result.TokenType, "bearer") || !validFCMToken(result.AccessToken) || result.ExpiresIn <= 0 || result.ExpiresIn > 86400 {
		return "", time.Time{}, &DeliveryError{Provider: "FCM OAuth", Code: "MalformedTokenResponse", Retryable: true, RetryAfter: 10 * time.Second}
	}
	return result.AccessToken, now.Add(time.Duration(result.ExpiresIn) * time.Second), nil
}

func validFCMToken(token string) bool {
	return len(token) > 0 && len(token) <= 4096 &&
		strings.IndexFunc(token, func(r rune) bool {
			return r < '!' || r > '~'
		}) < 0
}

func (f *FCM) rejectToken(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token == token {
		f.token = ""
	}
}

type fcmRequest struct {
	Message struct {
		Token   string            `json:"token"`
		Data    map[string]string `json:"data"`
		Android fcmAndroid        `json:"android"`
	} `json:"message"`
}

type fcmAndroid struct {
	Priority             string `json:"priority"`
	TTL                  string `json:"ttl"`
	CollapseKey          string `json:"collapse_key,omitempty"`
	BandwidthConstrained bool   `json:"bandwidth_constrained_ok,omitempty"`
}

func (f *FCM) request(m Message, deviceToken string, now time.Time) (fcmRequest, error) {
	var payload fcmRequest
	payload.Message.Token = deviceToken
	remaining := min(m.ExpiresAt.Sub(now), maxFCMTTL)
	if remaining <= 0 {
		return payload, errors.New("push message expired")
	}
	payload.Message.Android = fcmAndroid{
		Priority:             "HIGH",
		TTL:                  strconv.FormatInt(int64(remaining/time.Second), 10) + "s",
		BandwidthConstrained: f.bandwidthConstrained,
	}
	data := map[string]string{"flightId": m.FlightID}
	if m.binding != "" {
		data["binding"] = m.binding
	}
	if m.Title != "" {
		data["title"] = m.Title
	}
	if m.Body != "" {
		data["body"] = m.Body
	}
	if m.Kind == Alert {
		data["type"] = "alert"
		if m.ConnectionInboundID != "" {
			data["connectionInboundId"] = m.ConnectionInboundID
		}
		if m.CollapseID != "" {
			data["eventId"] = m.CollapseID
		}
	} else {
		data["type"] = "live"
		data["event"] = string(m.Kind)
		state := m.State
		data["status"] = state.Status
		data["origin"] = state.Origin
		data["destination"] = state.Destination
		data["departureEpoch"] = strconv.FormatInt(state.DepartureEpoch, 10)
		data["arrivalEpoch"] = strconv.FormatInt(state.ArrivalEpoch, 10)
		data["observedEpoch"] = strconv.FormatInt(state.ObservedEpoch, 10)
		for key, value := range map[string]string{
			"flightNumber":      state.FlightNumber,
			"departureGate":     state.DepartureGate,
			"arrivalGate":       state.ArrivalGate,
			"departureTerminal": state.DepartureTerminal,
			"arrivalTerminal":   state.ArrivalTerminal,
			"seat":              state.Seat,
		} {
			if value != "" {
				data[key] = value
			}
		}
		if m.Kind == LiveUpdate {
			payload.Message.Android.CollapseKey = m.CollapseID
		}
	}
	if _, err := encodePayload(data); err != nil {
		return payload, err
	}
	payload.Message.Data = data
	return payload, nil
}

func (f *FCM) Send(ctx context.Context, deviceToken string, message Message) error {
	if !validFCMToken(deviceToken) || strings.ContainsFunc(deviceToken, unicode.IsSpace) {
		return &DeliveryError{Provider: "FCM", Code: "MalformedDeviceToken", InvalidToken: true}
	}
	m, err := message.prepare(time.Now())
	if err != nil {
		return err
	}
	payload, err := f.request(m, deviceToken, time.Now())
	if err != nil {
		return err
	}
	token, err := f.accessToken(ctx)
	if err != nil {
		return err
	}
	// The first OAuth exchange can consume part of the delivery window.
	remaining := min(time.Until(m.ExpiresAt), maxFCMTTL)
	if remaining <= 0 {
		return errors.New("push message expired")
	}
	payload.Message.Android.TTL = strconv.FormatInt(int64(remaining/time.Second), 10) + "s"
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+token)
	response, err := post(ctx, f.client, "FCM", f.endpoint, headers, body)
	if err != nil {
		if failure, ok := err.(*DeliveryError); ok && failure.Retryable {
			failure.RetryAfter = 10 * time.Second
		}
		return err
	}
	if response.status == http.StatusOK {
		return nil
	}
	if response.status == http.StatusUnauthorized {
		// Refresh credentials for the next send. FCM advises aborting the
		// current request on 401, and the target registration can still be valid.
		f.rejectToken(token)
	}
	return fcmRejection(response)
}

func fcmRejection(response providerResponse) *DeliveryError {
	var result struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				Type string `json:"@type"`
				Code string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(response.body, &result)
	code := result.Error.Status
	invalidToken := false
	for _, detail := range result.Error.Details {
		if detail.Type == "type.googleapis.com/google.firebase.fcm.v1.FcmError" && detail.Code != "" {
			code = detail.Code
			invalidToken = detail.Code == "UNREGISTERED" && response.status == http.StatusNotFound
			break
		}
	}
	rejection := fcmFailure("FCM", safeReason(code), response)
	rejection.InvalidToken = invalidToken
	return rejection
}

func fcmFailure(provider, code string, response providerResponse) *DeliveryError {
	failure := &DeliveryError{
		Provider:   provider,
		Code:       code,
		Status:     response.status,
		Retryable:  response.status >= 500 || response.status == http.StatusTooManyRequests,
		RetryAfter: response.retryAfter,
	}
	if response.status == http.StatusTooManyRequests {
		failure.RetryAfter = max(failure.RetryAfter, time.Minute)
	} else if failure.Retryable {
		failure.RetryAfter = max(failure.RetryAfter, 10*time.Second)
	}
	return failure
}
