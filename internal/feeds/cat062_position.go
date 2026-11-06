package feeds

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/bits"
	"slices"
	"time"
)

const cat062PositionEdition = "1.21"

var errInvalidCAT062Position = fmt.Errorf("%w: invalid EUROCONTROL CAT062 1.21 position report", ErrInvalidQuery)

// CAT062PositionBinding identifies one authorized surveillance track and the
// independently verified provider flight it represents on the source UTC day.
type CAT062PositionBinding struct {
	Version            int       `json:"version"`
	Edition            string    `json:"edition"`
	DisplayAllowed     bool      `json:"displayAllowed"`
	ObservedAirborne   bool      `json:"observedAirborne"`
	ProviderFlightID   string    `json:"providerFlightId"`
	OriginICAO         string    `json:"originICAO"`
	DestinationICAO    string    `json:"destinationICAO"`
	ScheduledDeparture time.Time `json:"scheduledDeparture"`
	ActualTakeoff      time.Time `json:"actualTakeoff"`
	TrackDate          string    `json:"trackDate"`
	SAC                *uint8    `json:"sac"`
	SIC                *uint8    `json:"sic"`
	ServiceID          *uint8    `json:"serviceId,omitempty"`
	TrackNumber        *uint16   `json:"trackNumber"`
	AircraftAddress    string    `json:"aircraftAddress"`
}

// DecodeCAT062Positions converts CAT062 surveillance state vectors. A valid
// input without a fresh, usable observation of the bound track returns ErrNotFound.
func DecodeCAT062Positions(input io.Reader, binding CAT062PositionBinding, now time.Time) (RadarPositionReport, error) {
	day, dateErr := time.Parse(time.DateOnly, binding.TrackDate)
	address, addressErr := hex.DecodeString(binding.AircraftAddress)
	if binding.Version != 1 || binding.Edition != cat062PositionEdition || !binding.DisplayAllowed || !binding.ObservedAirborne ||
		!providerIdentifier.MatchString(binding.ProviderFlightID) ||
		!aeroAlertAirport.MatchString(binding.OriginICAO) || !aeroAlertAirport.MatchString(binding.DestinationICAO) ||
		binding.OriginICAO == "ZZZZ" || binding.OriginICAO == "AFIL" || binding.DestinationICAO == "ZZZZ" || binding.DestinationICAO == "AFIL" ||
		binding.ScheduledDeparture.Year() < 2000 || binding.ScheduledDeparture.Year() > 2100 ||
		binding.ActualTakeoff.Year() < 2000 || binding.ActualTakeoff.Year() > 2100 || binding.ActualTakeoff.After(now) ||
		dateErr != nil || day.Year() < 2000 || day.Year() > 2100 ||
		binding.SAC == nil || binding.SIC == nil || binding.TrackNumber == nil || addressErr != nil || len(address) != 3 ||
		bytes.Equal(address, []byte{0, 0, 0}) || bytes.Equal(address, []byte{0xff, 0xff, 0xff}) {
		return RadarPositionReport{}, errInvalidCAT062Position
	}
	records, err := decodeCAT062Wire(input)
	if err != nil {
		return RadarPositionReport{}, err
	}
	fixes := make(map[time.Time]Position)
	for _, record := range records {
		if !record.matchesPositionBinding(binding, address) || !record.isObservedAirborne() {
			continue
		}
		eligible, err := cat062ReferenceEligible(record.items[cat062REField])
		if err != nil {
			return RadarPositionReport{}, err
		}
		if !eligible {
			continue
		}
		fix, err := record.radarPosition(day, binding.ActualTakeoff, now)
		if err != nil {
			return RadarPositionReport{}, err
		}
		if fix == nil {
			continue
		}
		if previous, exists := fixes[fix.At]; exists {
			if previous.Latitude != fix.Latitude || previous.Longitude != fix.Longitude ||
				!sameRadarMeasurement(previous.AltitudeFeet, fix.AltitudeFeet) ||
				!sameRadarMeasurement(previous.GroundSpeedKnots, fix.GroundSpeedKnots) ||
				!sameRadarMeasurement(previous.VerticalRateFPM, fix.VerticalRateFPM) {
				return RadarPositionReport{}, errInvalidCAT062Position
			}
			continue
		}
		if len(fixes) == maxRadarPositionFixes {
			return RadarPositionReport{}, errInvalidCAT062Position
		}
		fixes[fix.At] = *fix
	}
	if len(fixes) == 0 {
		return RadarPositionReport{}, ErrNotFound
	}
	positions := make([]Position, 0, len(fixes))
	for _, fix := range fixes {
		positions = append(positions, fix)
	}
	slices.SortFunc(positions, func(a, b Position) int { return a.At.Compare(b.At) })
	return RadarPositionReport{
		ProviderFlightID: binding.ProviderFlightID, OriginICAO: binding.OriginICAO, DestinationICAO: binding.DestinationICAO,
		ScheduledDeparture: binding.ScheduledDeparture.UTC(), ActualTakeoff: binding.ActualTakeoff.UTC(), Positions: positions,
	}, nil
}

func (r cat062Record) matchesPositionBinding(binding CAT062PositionBinding, address []byte) bool {
	source, track, service := r.items[1], r.items[12], r.items[3]
	return len(source) == 2 && source[0] == *binding.SAC && source[1] == *binding.SIC &&
		len(track) == 2 && binary.BigEndian.Uint16(track) == *binding.TrackNumber &&
		(binding.ServiceID == nil || len(service) == 1 && service[0] == *binding.ServiceID) &&
		bytes.Equal(r.aircraft[1], address)
}

func (r cat062Record) isObservedAirborne() bool {
	status := r.items[13]
	// I062/080 rejects tentative, simulated, ending, inconsistent, coasted,
	// potentially false, flight-plan-only, and surface tracks.
	if len(status) == 0 || status[0]&0x02 != 0 || len(status) > 1 && status[1]&0xc8 != 0 ||
		len(status) > 3 && status[3]&0x80 != 0 || len(status) > 4 && status[4]&0x06 != 0 ||
		len(status) > 5 && status[5]&0x10 != 0 || len(r.items[23]) != 0 {
		return false
	}
	// I062/380 COM/ACAS flight status and ADS-B ground bit can independently
	// disqualify a track. The binding supplies positive evidence of airborne status.
	if com := r.aircraft[10]; len(com) == 2 {
		flightStatus := (binary.BigEndian.Uint16(com) >> 10) & 7
		if flightStatus == 1 || flightStatus == 3 {
			return false
		}
	}
	adsb := r.aircraft[11]
	return len(adsb) != 2 || binary.BigEndian.Uint16(adsb)&0x0200 == 0
}

// Appendix A 1.4 puts supplementary track status after three optional
// subfields. The wire decoder already checked the enclosing item length.
func cat062ReferenceEligible(ref []byte) (bool, error) {
	if len(ref) == 0 {
		return true, nil
	}
	if len(ref) < 2 {
		return false, errInvalidCAT062Position
	}
	indicator := ref[1]
	cursor := cat062Cursor{remaining: ref[2:]}
	for i, width := range []int{5, 3} {
		if indicator&(0x80>>i) != 0 {
			cursor.repeated(width)
		}
	}
	if indicator&0x20 != 0 {
		cursor.take(4)
	}
	var status []byte
	if indicator&0x10 != 0 {
		status = cursor.extended(1, 3)
	}
	// Remaining expansion items are opaque to the relay. Each advertised
	// item must still have space in the enclosing expansion.
	suffix := bits.OnesCount8(indicator & 0x0f)
	if cursor.invalid || len(cursor.remaining) < suffix || suffix == 0 && len(cursor.remaining) != 0 {
		return false, errInvalidCAT062Position
	}
	if len(status) == 0 {
		return true, nil
	}
	primary := status[0]
	var extension byte
	if len(status) > 1 {
		extension = status[1]
	}
	// ATP identifies a non-ICAO address. DAD and DUP mark service-wide and
	// sensor duplicate addresses. CSX indicates extended coasting.
	if primary&0x10 != 0 && primary&0x0e != 0 || extension&0xc0 == 0xc0 ||
		extension&0x20 != 0 && extension&0x18 != 0 || extension&0x06 == 0x06 {
		return false, nil
	}
	return true, nil
}

func (r cat062Record) radarPosition(day, takeoff, now time.Time) (*Position, error) {
	trackTime, wgs84 := r.items[4], r.items[5]
	if len(trackTime) != 3 || len(wgs84) != 8 {
		return nil, nil
	}
	ticks := time.Duration(trackTime[0])<<16 | time.Duration(trackTime[1])<<8 | time.Duration(trackTime[2])
	at := day.Add(ticks * (time.Second / 128))
	if !at.Before(day.Add(24 * time.Hour)) {
		return nil, errInvalidCAT062Position
	}
	oldest := now.Add(-RadarPositionFreshness)
	if at.Before(takeoff) || at.Before(oldest) || at.After(now) {
		return nil, nil
	}
	const coordinateScale = 180.0 / (1 << 25)
	latitude := float64(int32(binary.BigEndian.Uint32(wgs84[:4]))) * coordinateScale
	longitude := float64(int32(binary.BigEndian.Uint32(wgs84[4:]))) * coordinateScale
	if latitude < -90 || latitude > 90 || longitude < -180 || longitude >= 180 {
		return nil, errInvalidCAT062Position
	}
	position := &Position{Latitude: latitude, Longitude: longitude, At: at, Source: Eurocontrol, Kind: "radar"}
	if velocity := r.items[7]; len(velocity) == 4 {
		x := float64(int16(binary.BigEndian.Uint16(velocity[:2]))) * 0.25
		y := float64(int16(binary.BigEndian.Uint16(velocity[2:]))) * 0.25
		knots := math.Hypot(x, y) * (3600.0 / 1852)
		if radarPositionRange(&knots, 0, 2500) {
			position.GroundSpeedKnots = &knots
		}
	}
	if altitude := r.items[18]; len(altitude) == 2 {
		feet := float64(int16(binary.BigEndian.Uint16(altitude))) * 6.25
		if radarPositionRange(&feet, -1500, 100000) {
			position.AltitudeFeet = &feet
		}
	}
	if rate := r.items[20]; len(rate) == 2 {
		fpm := float64(int16(binary.BigEndian.Uint16(rate))) * 6.25
		if radarPositionRange(&fpm, -25000, 25000) {
			position.VerticalRateFPM = &fpm
		}
	}
	return position, nil
}
