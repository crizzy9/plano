// Package demodata supplies an isolated synthetic schedule for local evaluation.
package demodata

import (
	"context"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

type Source struct{}

const (
	demoSource      = "plano_demo"
	demoLandingLead = 10 * time.Minute
)

func (s Source) Search(ctx context.Context, number string, date time.Time) ([]feeds.Flight, error) {
	number = strings.ToUpper(strings.Join(strings.Fields(number), ""))
	if number != "UA123" && number != "UA456" {
		return []feeds.Flight{}, nil
	}
	flight, err := s.Get(ctx, "demo-"+number+"-"+date.Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	return []feeds.Flight{flight}, nil
}

func (Source) Get(_ context.Context, id string) (feeds.Flight, error) {
	return flightAt(id, time.Now().UTC())
}

func flightAt(id string, now time.Time) (feeds.Flight, error) {
	if !strings.HasPrefix(id, "demo-UA123-") && !strings.HasPrefix(id, "demo-UA456-") {
		return feeds.Flight{}, feeds.ErrNotFound
	}
	number := id[5:10]
	day, err := time.Parse(time.DateOnly, id[11:])
	if err != nil {
		return feeds.Flight{}, feeds.ErrNotFound
	}
	depart, arrive := day.Add(15*time.Hour), day.Add(20*time.Hour)
	origin, destination, depGate, arrGate, distance := "SFO", "EWR", "F12", "C82", 2565
	originZone, destinationZone := "America/Los_Angeles", "America/New_York"
	if number == "UA456" {
		depart, arrive = day.Add(22*time.Hour), day.Add(29*time.Hour)
		origin, destination, depGate, arrGate, distance = "EWR", "LHR", "C84", "B39", 3465
		originZone, destinationZone = "America/New_York", "Europe/London"
	}
	f := feeds.Flight{ID: id, Number: number, Origin: origin, Destination: destination, Status: "scheduled", ScheduledDeparture: &depart, ScheduledArrival: &arrive, DepartureGate: depGate, ArrivalGate: arrGate, DepartureTerminal: string(depGate[0]), ArrivalTerminal: string(arrGate[0]), TailNumber: "DEMO", AircraftType: "B789", DistanceMiles: &distance, ObservedAt: now, Source: demoSource}
	f.OriginTimeZone, f.DestinationTimeZone = originZone, destinationZone
	if !now.Before(depart.Add(-40 * time.Minute)) {
		f.Status = "boarding"
	}
	if !now.Before(depart) {
		f.Status, f.ActualDeparture = "taxi_out", &depart
	}
	takeoff, landing := depart.Add(12*time.Minute), arrive.Add(-demoLandingLead)
	if !now.Before(takeoff) {
		f.Status, f.ActualTakeoff = "airborne", &takeoff
	}
	if !now.Before(landing) {
		f.Status, f.ActualLanding = "landed", &landing
	}
	if !now.Before(arrive) {
		f.Status, f.ActualArrival, f.Baggage = "arrived", &arrive, "7"
	}
	return f, nil
}

func (Source) Track(_ context.Context, id string) ([]feeds.Position, error) {
	return trackAt(id, time.Now().UTC())
}

func trackAt(id string, now time.Time) ([]feeds.Position, error) {
	flight, err := flightAt(id, now)
	if err != nil {
		return nil, err
	}
	if flight.ActualTakeoff == nil {
		return []feeds.Position{}, nil
	}

	// Synthetic waypoints follow the geography of the two demo routes.
	waypoints := [3][2]float64{{37.62, -122.38}, {42, -98}, {40.69, -74.17}}
	if flight.Number == "UA456" {
		waypoints = [3][2]float64{{40.69, -74.17}, {52, -40}, {51.47, -0.45}}
	}
	takeoff, landing := *flight.ActualTakeoff, flight.ScheduledArrival.Add(-demoLandingLead)
	end := now
	if landing.Before(end) {
		end = landing
	}
	const step = time.Minute
	positions := make([]feeds.Position, 0, 1+int(end.Sub(takeoff)/step))
	for at := takeoff; !at.After(end); at = at.Add(step) {
		elapsed, remaining := at.Sub(takeoff), landing.Sub(at)
		progress := 2 * float64(elapsed) / float64(landing.Sub(takeoff))
		segment := min(int(progress), len(waypoints)-2)
		fraction := progress - float64(segment)
		start, finish := waypoints[segment], waypoints[segment+1]

		const cruiseAltitude, cruiseSpeed = 36000.0, 460.0
		const climbDuration, descentDuration = 20 * time.Minute, 30 * time.Minute
		altitude, speed, verticalRate := cruiseAltitude, cruiseSpeed, 0.0
		switch {
		case elapsed < climbDuration:
			portion := float64(elapsed) / float64(climbDuration)
			altitude, speed = cruiseAltitude*portion, 175+(cruiseSpeed-175)*portion
			verticalRate = cruiseAltitude / climbDuration.Minutes()
		case remaining <= descentDuration:
			portion := float64(remaining) / float64(descentDuration)
			altitude, speed = cruiseAltitude*portion, 150+(cruiseSpeed-150)*portion
			verticalRate = -cruiseAltitude / descentDuration.Minutes()
		}
		positions = append(positions, feeds.Position{
			Latitude: start[0] + (finish[0]-start[0])*fraction, Longitude: start[1] + (finish[1]-start[1])*fraction,
			AltitudeFeet: &altitude, GroundSpeedKnots: &speed, VerticalRateFPM: &verticalRate,
			At: at, Source: demoSource, Kind: "synthetic",
		})
	}
	return positions, nil
}

func (s Source) Rotation(ctx context.Context, flight feeds.Flight) ([]feeds.Flight, error) {
	current, err := s.Get(ctx, flight.ID)
	if err != nil {
		return nil, err
	}
	if current.Number != "UA456" {
		return []feeds.Flight{}, nil
	}
	inbound, err := s.Get(ctx, "demo-UA123-"+current.ScheduledDeparture.Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	return []feeds.Flight{inbound}, nil
}
