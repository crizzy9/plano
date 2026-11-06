// Package feeds normalizes licensed aviation data and public FAA advisories.
// Unknown provider values remain absent.
package feeds

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound     = errors.New("aviation data not found")
	ErrUnavailable  = errors.New("aviation source unavailable")
	ErrInvalidQuery = errors.New("invalid aviation data query")
)

type FlightSource interface {
	Search(context.Context, string, time.Time) ([]Flight, error)
	Get(context.Context, string) (Flight, error)
	Track(context.Context, string) ([]Position, error)
	Rotation(context.Context, Flight) ([]Flight, error)
}

type AdvisorySource interface {
	Advisories(context.Context) ([]Advisory, error)
}

type Flight struct {
	ID                  string `json:"id"`
	providerID          string
	Number              string     `json:"number"`
	ICAONumber          string     `json:"icaoNumber,omitempty"`
	ATCIdent            string     `json:"atcIdent,omitempty"`
	Origin              string     `json:"origin"`
	Destination         string     `json:"destination"`
	OriginICAO          string     `json:"originICAO,omitempty"`
	DestinationICAO     string     `json:"destinationICAO,omitempty"`
	OriginTimeZone      string     `json:"originTimeZone,omitempty"`
	DestinationTimeZone string     `json:"destinationTimeZone,omitempty"`
	Status              string     `json:"status"`
	ProviderStatus      string     `json:"providerStatus,omitempty"`
	ScheduledDeparture  *time.Time `json:"scheduledDeparture,omitempty"`
	ScheduledArrival    *time.Time `json:"scheduledArrival,omitempty"`
	EstimatedDeparture  *time.Time `json:"estimatedDeparture,omitempty"`
	EstimatedArrival    *time.Time `json:"estimatedArrival,omitempty"`
	ActualDeparture     *time.Time `json:"actualDeparture,omitempty"`
	ActualTakeoff       *time.Time `json:"actualTakeoff,omitempty"`
	ActualLanding       *time.Time `json:"actualLanding,omitempty"`
	ActualArrival       *time.Time `json:"actualArrival,omitempty"`
	DepartureGate       string     `json:"departureGate,omitempty"`
	ArrivalGate         string     `json:"arrivalGate,omitempty"`
	DepartureTerminal   string     `json:"departureTerminal,omitempty"`
	ArrivalTerminal     string     `json:"arrivalTerminal,omitempty"`
	TailNumber          string     `json:"tailNumber,omitempty"`
	AircraftType        string     `json:"aircraftType,omitempty"`
	AircraftAge         *int       `json:"aircraftAge,omitempty"`
	DistanceMiles       *int       `json:"distanceMiles,omitempty"`
	Baggage             string     `json:"baggage,omitempty"`
	InboundID           string     `json:"inboundId,omitempty"`
	ObservedAt          time.Time  `json:"observedAt"`
	Source              string     `json:"source"`
}

type Position struct {
	Latitude         float64   `json:"latitude"`
	Longitude        float64   `json:"longitude"`
	AltitudeFeet     *float64  `json:"altitudeFeet,omitempty"`
	GroundSpeedKnots *float64  `json:"groundSpeedKnots,omitempty"`
	VerticalRateFPM  *float64  `json:"verticalRateFpm,omitempty"`
	VerticalTrend    string    `json:"verticalTrend,omitempty"`
	Heading          *float64  `json:"heading,omitempty"`
	At               time.Time `json:"at"`
	Source           string    `json:"source"`
	Kind             string    `json:"kind,omitempty"`
}

type Advisory struct {
	ID           string     `json:"id"`
	SupersedesID string     `json:"supersedesId,omitempty"`
	Airport      string     `json:"airport,omitempty"`
	Kind         string     `json:"kind"`
	Reason       string     `json:"reason"`
	Source       string     `json:"source"`
	StartsAt     *time.Time `json:"startsAt,omitempty"`
	EndsAt       *time.Time `json:"endsAt,omitempty"`
	ObservedAt   time.Time  `json:"observedAt"`
	DelayMinutes *int       `json:"delayMinutes,omitempty"`
	Area         string     `json:"area,omitempty"`
	Direction    string     `json:"direction,omitempty"`
	RawReason    string     `json:"rawReason,omitempty"`
}
