// Package demodata supplies an isolated synthetic schedule for local evaluation.
package demodata

import (
	"context"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

type Source struct{}

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
	if number == "UA456" {
		depart, arrive = day.Add(22*time.Hour), day.Add(29*time.Hour)
		origin, destination, depGate, arrGate, distance = "EWR", "LHR", "C84", "B39", 3465
	}
	now := time.Now().UTC()
	f := feeds.Flight{ID: id, Number: number, Origin: origin, Destination: destination, Status: "scheduled", ScheduledDeparture: &depart, ScheduledArrival: &arrive, DepartureGate: depGate, ArrivalGate: arrGate, DepartureTerminal: string(depGate[0]), ArrivalTerminal: string(arrGate[0]), TailNumber: "DEMO", AircraftType: "B789", DistanceMiles: &distance, ObservedAt: now, Source: "plano_demo"}
	if !now.Before(depart.Add(-40 * time.Minute)) {
		f.Status = "boarding"
	}
	if !now.Before(depart) {
		f.Status, f.ActualDeparture = "taxi_out", &depart
	}
	takeoff, landing := depart.Add(12*time.Minute), arrive.Add(-10*time.Minute)
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

func (Source) Track(context.Context, string) ([]feeds.Position, error) {
	return []feeds.Position{}, nil
}
func (Source) Rotation(context.Context, feeds.Flight) ([]feeds.Flight, error) {
	return []feeds.Flight{}, nil
}
