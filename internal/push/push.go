// Package push sends flight alerts and live status over APNs and FCM.
package push

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Kind string

const (
	Alert      Kind = "alert"
	LiveStart  Kind = "start"
	LiveUpdate Kind = "update"
	LiveEnd    Kind = "end"

	// Keep notifications small enough to carry useful state without a follow-up
	// API call, including on networks that only allow push traffic.
	maxPayloadBytes  = 2048
	maxResponseBytes = 16 << 10
)

type Target struct {
	Platform string
	Token    string
	Binding  string
}

type LiveState struct {
	FlightID          string `json:"flightId"`
	FlightNumber      string `json:"flightNumber,omitempty"`
	Status            string `json:"status"`
	Origin            string `json:"origin"`
	Destination       string `json:"destination"`
	DepartureGate     string `json:"departureGate,omitempty"`
	ArrivalGate       string `json:"arrivalGate,omitempty"`
	DepartureTerminal string `json:"departureTerminal,omitempty"`
	ArrivalTerminal   string `json:"arrivalTerminal,omitempty"`
	Seat              string `json:"seat,omitempty"`
	DepartureEpoch    int64  `json:"departureEpoch"`
	ArrivalEpoch      int64  `json:"arrivalEpoch"`
	ObservedEpoch     int64  `json:"observedEpoch"`
}

// AdvisoryWindow records the exact occurrence a queued alert describes.
// It is stored with the outbox message for revalidation before delivery.
type AdvisoryWindow struct {
	Airport   string
	Direction string
	StartsAt  time.Time
	EndsAt    time.Time
	Kind      string `json:",omitempty"`
}

type Message struct {
	Kind                Kind
	FlightID            string
	ConnectionInboundID string `json:",omitempty"`
	AlertKind           string `json:",omitempty"`
	AlertPhase          string `json:",omitempty"`
	Title               string
	Body                string
	CollapseID          string
	APNsCollapseID      string          `json:",omitempty"`
	AdvisoryWindow      *AdvisoryWindow `json:",omitempty"`
	ATFMRevision        string          `json:",omitempty"` // Private outbox evidence.
	InboundLegID        string          `json:",omitempty"` // Private outbox evidence.
	State               *LiveState
	Timestamp           time.Time
	ExpiresAt           time.Time
	StaleAt             time.Time
	DismissAt           time.Time
	RequestUpdateToken  bool // Enable for iOS 18 or later push-to-start registrations.

	// LiveObservedNs preserves the source observation for Android delivery ordering.
	LiveObservedNs int64 `json:",omitempty"`
	// AndroidLiveHeartbeat marks a routine refresh for immediate delivery only.
	AndroidLiveHeartbeat bool   `json:",omitempty"`
	binding              string // Opaque session binding from the destination device registration.
}

type Sender struct {
	APNs *APNs
	FCM  *FCM
}

type DeliveryError struct {
	Provider           string
	Code               string
	Status             int
	Retryable          bool
	RetryAfter         time.Duration
	InvalidToken       bool
	TokenInvalidatedAt time.Time
}

func (e *DeliveryError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s push: %s (HTTP %d)", e.Provider, e.Code, e.Status)
	}
	return fmt.Sprintf("%s push: %s", e.Provider, e.Code)
}

func (s Sender) Send(ctx context.Context, target Target, m Message) error {
	m.binding = target.Binding
	switch target.Platform {
	case "ios":
		if s.APNs == nil {
			return &DeliveryError{Provider: "APNs", Code: "NotConfigured", Retryable: true}
		}
		return s.APNs.Send(ctx, target.Token, m)
	case "android":
		if s.FCM == nil {
			return &DeliveryError{Provider: "FCM", Code: "NotConfigured", Retryable: true}
		}
		return s.FCM.Send(ctx, target.Token, m)
	default:
		return errors.New("unsupported push target platform")
	}
}

func (m Message) prepare(now time.Time) (Message, error) {
	switch m.Kind {
	case Alert:
		if strings.TrimSpace(m.Title) == "" || strings.TrimSpace(m.Body) == "" {
			return m, errors.New("push alert requires title and body")
		}
	case LiveStart, LiveUpdate, LiveEnd:
		if m.State == nil {
			return m, errors.New("live push requires content state")
		}
		state := *m.State
		if m.FlightID == "" {
			m.FlightID = state.FlightID
		}
		if state.FlightID == "" {
			state.FlightID = m.FlightID
		}
		if state.FlightID != m.FlightID || state.Status == "" {
			return m, errors.New("invalid live push flight state")
		}
		m.State = &state
	default:
		return m, errors.New("unsupported push kind")
	}
	if m.LiveObservedNs != 0 && (m.Kind == Alert || m.LiveObservedNs < int64(time.Second) ||
		m.LiveObservedNs/int64(time.Second) != m.State.ObservedEpoch) {
		return m, errors.New("invalid live push observation time")
	}
	if strings.TrimSpace(m.FlightID) == "" {
		return m, errors.New("push requires a flight ID")
	}
	if m.ConnectionInboundID != "" && (m.Kind != Alert || m.ConnectionInboundID == m.FlightID ||
		!validConnectionFlightID(m.ConnectionInboundID) || !validConnectionFlightID(m.FlightID)) {
		return m, errors.New("invalid push connection route")
	}
	if m.AlertKind != "" && (m.Kind != Alert || !alertLabelPattern.MatchString(m.AlertKind)) {
		return m, errors.New("invalid push alert kind")
	}
	if m.AlertPhase != "" && (m.AlertKind == "" || !alertLabelPattern.MatchString(m.AlertPhase)) {
		return m, errors.New("invalid push alert phase")
	}
	if !validCollapseID(m.CollapseID) {
		return m, errors.New("invalid push collapse ID")
	}
	if m.APNsCollapseID != "" && (m.Kind != Alert || m.CollapseID == "" || !validCollapseID(m.APNsCollapseID)) {
		return m, errors.New("invalid APNs alert collapse ID")
	}
	if m.Timestamp.IsZero() {
		m.Timestamp = now
	}
	if m.State != nil && m.Kind != Alert && m.State.ObservedEpoch == 0 {
		m.State.ObservedEpoch = m.Timestamp.Unix()
	}
	if m.ExpiresAt.IsZero() {
		m.ExpiresAt = m.Timestamp.Add(15 * time.Minute)
		if m.Kind == LiveStart {
			m.ExpiresAt = m.Timestamp.Add(2 * time.Minute)
		}
	}
	if !m.ExpiresAt.After(now) {
		return m, errors.New("push message expired")
	}
	if m.AlertKind != "" {
		observed := m.Timestamp.UnixNano()
		if observed <= 0 || !time.Unix(0, observed).Equal(m.Timestamp) || m.ExpiresAt.UnixMilli() <= m.Timestamp.UnixMilli() {
			return m, errors.New("invalid push alert delivery window")
		}
	}
	return m, nil
}

var connectionFlightIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
var alertLabelPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func validCollapseID(id string) bool {
	return len(id) <= 64 && strings.IndexFunc(id, unicode.IsControl) < 0
}

func validConnectionFlightID(id string) bool {
	return id != "." && id != ".." && connectionFlightIDPattern.MatchString(id)
}

func encodePayload(v any) ([]byte, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxPayloadBytes {
		return nil, errors.New("push payload exceeds Plano's 2048-byte limit")
	}
	return payload, nil
}

func makeClient(http2Only bool) *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(!http2Only)
	protocols.SetHTTP2(true)
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		Protocols:             protocols,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type providerResponse struct {
	status     int
	body       []byte
	retryAfter time.Duration
}

func post(ctx context.Context, client *http.Client, provider, endpoint string, headers http.Header, body []byte) (providerResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providerResponse{}, &DeliveryError{Provider: provider, Code: "InvalidEndpoint"}
	}
	req.Header = headers
	resp, err := client.Do(req)
	if err != nil {
		// Transport errors may contain device tokens in URLs.
		if ctx.Err() != nil {
			return providerResponse{}, ctx.Err()
		}
		return providerResponse{}, &DeliveryError{Provider: provider, Code: "TransportError", Retryable: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		// Still honor the HTTP status when a response body is unavailable.
		raw = nil
	}
	return providerResponse{
		status:     resp.StatusCode,
		body:       raw,
		retryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}, nil
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}

var reasonPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z_0-9]{0,47}$`)

func safeReason(reason string) string {
	if reasonPattern.MatchString(reason) {
		return reason
	}
	return "ProviderRejected"
}

func unsignedJWT(header, claims any) string {
	// Both arguments are internally constructed structs or string/int maps.
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
}

func signedJWT(unsigned string, signature []byte) string {
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}
