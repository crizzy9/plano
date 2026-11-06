package feeds

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var airportCode = regexp.MustCompile(`^[A-Z0-9]{2,7}$\z`)

// Airport contains the static metadata available from AeroAPI. Coordinate
// pointers distinguish an omitted coordinate from one on the equator or meridian.
type Airport struct {
	Code      string   `json:"code"`
	ICAO      string   `json:"icao,omitempty"`
	IATA      string   `json:"iata,omitempty"`
	Name      string   `json:"name"`
	City      string   `json:"city,omitempty"`
	TimeZone  string   `json:"timeZone,omitempty"`
	Country   string   `json:"country,omitempty"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
}

func (a *AeroAPI) Airport(ctx context.Context, code string) (Airport, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !airportCode.MatchString(code) {
		return Airport{}, ErrInvalidQuery
	}
	var response struct {
		Code      string   `json:"airport_code"`
		ICAO      string   `json:"code_icao"`
		IATA      string   `json:"code_iata"`
		Name      string   `json:"name"`
		City      string   `json:"city"`
		TimeZone  string   `json:"timezone"`
		Country   string   `json:"country_code"`
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
	}
	if err := a.getJSON(ctx, "/airports/"+code, nil, &response); err != nil {
		return Airport{}, err
	}
	if !airportCode.MatchString(response.Code) {
		return Airport{}, fmt.Errorf("%w: %s returned an invalid airport", ErrUnavailable, aeroSource)
	}
	if response.Latitude != nil && (*response.Latitude < -90 || *response.Latitude > 90) ||
		response.Longitude != nil && (*response.Longitude < -180 || *response.Longitude > 180) {
		return Airport{}, fmt.Errorf("%w: %s returned invalid airport coordinates", ErrUnavailable, aeroSource)
	}
	return Airport(response), nil
}
