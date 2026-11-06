package feeds

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
)

// MaxAeroAlertBytes bounds the descriptions and other fields that are not needed
// to identify the flight. A gateway can apply the same limit at the HTTP edge.
const MaxAeroAlertBytes = 64 << 10

var aeroAlertEventCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$\z`)

// AeroAlert identifies an AeroAPI flight that needs an authoritative refresh.
// The subscription ID is shared by callbacks for different flight instances.
// AeroAPI's callback envelope has no delivery ID or ordering timestamp.
type AeroAlert struct {
	SubscriptionID int64
	FlightID       string
	EventCode      string
}

// DecodeAeroAlert accepts FlightAware's documented callback envelope. It does
// not treat the embedded flight object as a newer or complete flight snapshot.
// Gateways authenticate the request separately before decoding it.
func DecodeAeroAlert(input io.Reader) (AeroAlert, error) {
	raw, err := io.ReadAll(io.LimitReader(input, MaxAeroAlertBytes+1))
	if err != nil {
		return AeroAlert{}, fmt.Errorf("%w: could not read AeroAPI callback", ErrInvalidQuery)
	}
	if len(raw) > MaxAeroAlertBytes {
		return AeroAlert{}, fmt.Errorf("%w: AeroAPI callback exceeds %d bytes", ErrInvalidQuery, MaxAeroAlertBytes)
	}
	var wire struct {
		SubscriptionID int64  `json:"alert_id"`
		EventCode      string `json:"event_code"`
		Flight         *struct {
			ID      string `json:"fa_flight_id"`
			Blocked bool   `json:"blocked"`
			Error   string `json:"error"`
		} `json:"flight"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || wire.SubscriptionID <= 0 || !aeroAlertEventCode.MatchString(wire.EventCode) || wire.Flight == nil {
		return AeroAlert{}, fmt.Errorf("%w: invalid AeroAPI callback envelope", ErrInvalidQuery)
	}
	if wire.Flight.Blocked || wire.Flight.Error != "" {
		return AeroAlert{}, ErrNotFound
	}
	if !providerIdentifier.MatchString(wire.Flight.ID) {
		return AeroAlert{}, fmt.Errorf("%w: invalid AeroAPI callback flight ID", ErrInvalidQuery)
	}
	return AeroAlert{SubscriptionID: wire.SubscriptionID, FlightID: wire.Flight.ID, EventCode: wire.EventCode}, nil
}
