package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const readsbSource = "operator_readsb"

var (
	modeSHexPattern             = regexp.MustCompile(`^[0-9a-f]{6}$\z`)
	explicitCanadianMarkPattern = regexp.MustCompile(`^(?:C-[FGI]|CF-)[A-Z]{3}$\z`)
)

// ADSBReceiver reads a readsb aircraft.json snapshot from an operator's receiver.
type ADSBReceiver struct {
	endpoint string
	client   *http.Client
	now      func() time.Time
}

func NewADSBReceiver(endpoint string, client *http.Client) (*ADSBReceiver, error) {
	if !validFeedEndpoint(endpoint) {
		return nil, fmt.Errorf("%w: ADS-B receiver requires HTTPS or loopback HTTP without URL credentials", ErrInvalidQuery)
	}
	return &ADSBReceiver{endpoint: endpoint, client: feedClient(client), now: time.Now}, nil
}

type adsbObservation struct {
	hex, registration, callSign string
	canadianMark                string
	position                    Position
}

// ADSBSnapshot has only recent, directly measured aircraft positions and is
// indexed by transponder address and reported registration for reuse per poll.
type ADSBSnapshot struct {
	byAddress      map[string]adsbObservation
	byRegistration map[string]adsbObservation
}

func (r *ADSBReceiver) Snapshot(ctx context.Context) (ADSBSnapshot, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.endpoint, nil)
	if err != nil {
		return ADSBSnapshot{}, err
	}
	request.Header.Set("Accept", "application/json")
	body, err := responseBody(r.client, request, readsbSource)
	if err != nil {
		return ADSBSnapshot{}, err
	}
	return parseReadsb(body, r.now())
}

func parseReadsb(body []byte, now time.Time) (ADSBSnapshot, error) {
	var wire struct {
		Now      float64 `json:"now"`
		Aircraft []struct {
			Hex          string          `json:"hex"`
			Type         string          `json:"type"`
			CallSign     string          `json:"flight"`
			Registration string          `json:"r"`
			Flags        int             `json:"dbFlags"`
			Latitude     *float64        `json:"lat"`
			Longitude    *float64        `json:"lon"`
			PositionAge  *float64        `json:"seen_pos"`
			Ground       bool            `json:"ground"`
			Altitude     json.RawMessage `json:"alt_baro"`
			AltitudeGNSS *float64        `json:"alt_geom"`
			GroundSpeed  *float64        `json:"gs"`
			BaroRate     *float64        `json:"baro_rate"`
			GeomRate     *float64        `json:"geom_rate"`
			Heading      *float64        `json:"true_heading"`
		} `json:"aircraft"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || wire.Now <= 0 || wire.Now >= 1e11 || wire.Aircraft == nil {
		return ADSBSnapshot{}, fmt.Errorf("%w: %s returned an invalid snapshot", ErrUnavailable, readsbSource)
	}
	seconds, fraction := math.Modf(wire.Now)
	published := time.Unix(int64(seconds), int64(fraction*1e9)).UTC()
	if published.Before(now.Add(-time.Minute)) || published.After(now.Add(5*time.Second)) {
		return ADSBSnapshot{}, fmt.Errorf("%w: %s snapshot is stale or clock is out of sync", ErrUnavailable, readsbSource)
	}
	result := ADSBSnapshot{byAddress: make(map[string]adsbObservation), byRegistration: make(map[string]adsbObservation)}
	for _, row := range wire.Aircraft {
		hex := strings.ToLower(row.Hex)
		// Flags 1, 4, and 8 identify military, PIA, and LADD respectively.
		if row.Type != "adsb_icao" || row.Flags&(1|4|8) != 0 || !modeSHexPattern.MatchString(hex) ||
			row.Ground || string(row.Altitude) == `"ground"` ||
			row.Latitude == nil || row.Longitude == nil || *row.Latitude < -90 || *row.Latitude > 90 ||
			*row.Longitude < -180 || *row.Longitude > 180 || row.PositionAge == nil || *row.PositionAge < 0 || *row.PositionAge > 30 {
			continue
		}
		at := published.Add(-time.Duration(*row.PositionAge * float64(time.Second)))
		if at.Before(now.Add(-time.Minute)) {
			continue
		}
		altitude := row.AltitudeGNSS
		if len(row.Altitude) > 0 {
			var baro *float64
			if err := json.Unmarshal(row.Altitude, &baro); err == nil && baro != nil {
				altitude = baro
			}
		}
		verticalRate := row.BaroRate
		if verticalRate == nil {
			verticalRate = row.GeomRate
		}
		observation := adsbObservation{
			hex: hex, registration: normalizeRegistration(row.Registration), callSign: strings.ToUpper(strings.TrimSpace(row.CallSign)),
			canadianMark: explicitCanadianMark(row.Registration),
			position: Position{At: at, Latitude: *row.Latitude, Longitude: *row.Longitude, AltitudeFeet: altitude,
				GroundSpeedKnots: row.GroundSpeed, VerticalRateFPM: verticalRate, Heading: row.Heading, Source: readsbSource, Kind: "ads_b"},
		}
		storeObservation(result.byAddress, hex, observation)
		if observation.registration != "" {
			storeObservation(result.byRegistration, observation.registration, observation)
		}
	}
	return result, nil
}

func storeObservation(index map[string]adsbObservation, key string, incoming adsbObservation) {
	current, exists := index[key]
	switch {
	case !exists:
		index[key] = incoming
	case current.hex != incoming.hex || canadianMarkConflict(current.canadianMark, incoming.canadianMark):
		// Different transponders claimed the same registration. Do not guess.
		index[key] = adsbObservation{}
	case incoming.position.At.After(current.position.At):
		index[key] = incoming
	}
}

func normalizeRegistration(registration string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(registration), "-", ""))
}

func explicitCanadianMark(registration string) string {
	mark := strings.ToUpper(strings.TrimSpace(registration))
	if explicitCanadianMarkPattern.MatchString(mark) {
		return mark
	}
	return ""
}

func canadianMarkConflict(a, b string) bool {
	return a != "" && b != "" && a != b
}

// Match returns the position and normalized receiver address for a flight. A
// conflicting callsign or registration rules out a point even with a Mode S hit.
func (s ADSBSnapshot) Match(flight Flight, modeSHex string) (position Position, address string, ok bool) {
	if flight.Status != "taxi_out" && flight.Status != "airborne" && flight.Status != "landed" &&
		!(flight.Status == "diverted" && flight.ActualTakeoff != nil && flight.ActualLanding == nil && flight.ActualArrival == nil) {
		return Position{}, "", false
	}
	tail := normalizeRegistration(flight.TailNumber)
	if tail == "" {
		return Position{}, "", false
	}
	row := s.byRegistration[tail]
	if modeSHexPattern.MatchString(strings.ToLower(modeSHex)) {
		row = s.byAddress[strings.ToLower(modeSHex)]
	}
	if row.hex == "" || row.registration != "" && row.registration != tail ||
		canadianMarkConflict(row.canadianMark, explicitCanadianMark(flight.TailNumber)) {
		return Position{}, "", false
	}
	if row.callSign != "" && row.callSign != tail && row.callSign != strings.ToUpper(flight.ATCIdent) &&
		row.callSign != strings.ToUpper(flight.ICAONumber) && row.callSign != strings.ToUpper(flight.Number) {
		return Position{}, "", false
	}
	start := firstTime(flight.ActualDeparture, flight.ActualTakeoff, flight.EstimatedDeparture, flight.ScheduledDeparture)
	if start == nil || row.position.At.Before(start.Add(-5*time.Minute)) {
		return Position{}, "", false
	}
	return row.position, row.hex, true
}
