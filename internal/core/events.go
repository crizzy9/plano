package core

import (
	"fmt"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/push"
)

// Event is a user-visible change in a flight, separate from its latest snapshot.
type Event struct {
	ID               string    `json:"id"`
	Kind             string    `json:"kind"`
	Title            string    `json:"title"`
	Body             string    `json:"body"`
	At               time.Time `json:"at"`
	validUntil       *time.Time
	collapseID       string
	advisoryEvidence []forecastAdvisory
	advisoryWindow   *push.AdvisoryWindow
	inboundLegID     string
}

func changes(before, after feeds.Flight, anchors estimateAlertAnchors) ([]Event, estimateAlertAnchors) {
	events := make([]Event, 0, 8)
	add := func(kind, title, body string) {
		events = append(events, Event{Kind: kind, Title: title, Body: body, At: after.ObservedAt})
	}
	for _, item := range []struct{ kind, title, prior, current string }{
		{"tail", "Aircraft assigned", before.TailNumber, after.TailNumber},
		{"departure_gate", "Departure gate", before.DepartureGate, after.DepartureGate},
		{"arrival_gate", "Arrival gate", before.ArrivalGate, after.ArrivalGate},
		{"baggage", "Baggage carousel", before.Baggage, after.Baggage},
	} {
		if item.current == "" || item.current == item.prior {
			continue
		}
		title := item.title
		if item.prior != "" {
			title = map[string]string{"tail": "Aircraft changed", "departure_gate": "Departure gate changed", "arrival_gate": "Arrival gate changed", "baggage": "Baggage carousel changed"}[item.kind]
		}
		add(item.kind, title, fmt.Sprintf("%s · %s", after.Number, item.current))
	}
	anchors.appendChanges(before, after, add)
	for _, item := range []struct {
		kind, title    string
		prior, current *time.Time
	}{
		{"taxi_out", "Pushed back from gate", before.ActualDeparture, after.ActualDeparture},
		{"airborne", "Taken off", before.ActualTakeoff, after.ActualTakeoff},
		{"landed", "Landed", before.ActualLanding, after.ActualLanding},
		{"arrived", "Arrived at the gate", before.ActualArrival, after.ActualArrival},
	} {
		if item.current != nil && item.prior == nil {
			add(item.kind, item.title, after.Number)
		}
	}
	if before.Status != after.Status {
		title := map[string]string{"boarding": "Boarding", "taxi_out": "Pushed back from gate", "airborne": "Taken off", "landed": "Landed", "arrived": "Arrived at the gate", "cancelled": "Flight cancelled", "diverted": "Flight diverted"}[after.Status]
		found := false
		for _, event := range events {
			found = found || event.Kind == after.Status
		}
		if title != "" && !found {
			add(after.Status, title, after.Number)
		}
	}
	return events, anchors
}

func signedMinutes(delta time.Duration) string {
	minutes := int(delta.Minutes())
	if minutes < 0 {
		return fmt.Sprintf("%d min earlier", -minutes)
	}
	return fmt.Sprintf("%d min later", minutes)
}

func firstTime(values ...*time.Time) *time.Time {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func effectiveDeparture(f feeds.Flight) *time.Time {
	return firstTime(f.ActualDeparture, f.EstimatedDeparture, f.ScheduledDeparture)
}

func effectiveArrival(f feeds.Flight) *time.Time {
	return firstTime(f.ActualArrival, f.EstimatedArrival, f.ScheduledArrival)
}

func finished(f feeds.Flight) bool {
	return f.Status == "arrived" || f.Status == "cancelled" || f.Status == "diverted" && f.ActualArrival != nil
}
