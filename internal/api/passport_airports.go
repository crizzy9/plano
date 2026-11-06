package api

import (
	_ "embed"
	"encoding/csv"
	"strconv"
	"strings"
	"sync"

	"github.com/crizzy9/plano/internal/core"
)

//go:embed passport_airports.csv
var passportAirportsCSV string

var passportAirportCatalog = sync.OnceValue(func() map[string]core.AirportCoordinates {
	reader := csv.NewReader(strings.NewReader(passportAirportsCSV))
	reader.FieldsPerRecord = 3
	reader.ReuseRecord = true
	points := make(map[string]core.AirportCoordinates)
	for {
		row, err := reader.Read()
		if err != nil {
			return points
		}
		lat, latErr := strconv.ParseFloat(row[1], 64)
		lon, lonErr := strconv.ParseFloat(row[2], 64)
		if latErr == nil && lonErr == nil && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 {
			points[row[0]] = core.AirportCoordinates{Latitude: lat, Longitude: lon}
		}
	}
})

func passportArchiveCoordinates(airport passportAirport) *core.AirportCoordinates {
	points := passportAirportCatalog()
	if point, ok := points[airport.primary]; ok {
		return &point
	}
	if point, ok := points[airport.alternate]; ok {
		return &point
	}
	return nil
}
