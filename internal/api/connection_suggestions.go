package api

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/crizzy9/plano/internal/core"
)

const (
	connectionSuggestionHorizon    = 7 * 24 * time.Hour
	connectionSuggestionLimit      = 8
	connectionSuggestionsPerOnward = 2
)

type connectionSuggestion struct {
	Inbound            string    `json:"inbound"`
	Outbound           string    `json:"outbound"`
	InboundNumber      string    `json:"inboundNumber"`
	OutboundNumber     string    `json:"outboundNumber"`
	Airport            string    `json:"airport"`
	TimeZone           string    `json:"timeZone,omitempty"`
	ScheduledArrival   time.Time `json:"scheduledArrival"`
	ScheduledDeparture time.Time `json:"scheduledDeparture"`
	ScheduledMinutes   int       `json:"scheduledMinutes"`
	Source             string    `json:"source"`
	departure          time.Time
}

func (s Server) connectionSuggestions(w http.ResponseWriter, r *http.Request, owner string) error {
	now := time.Now()
	latest := now.Add(connectionSuggestionHorizon)
	candidates, err := s.Store.ConnectionSuggestionFlights(r.Context(), owner, now, latest)
	if err != nil {
		return err
	}
	respond(w, http.StatusOK, map[string][]connectionSuggestion{
		"suggestions": rankConnectionSuggestions(candidates, now, latest),
	})
	return nil
}

func rankConnectionSuggestions(candidates []core.ConnectionSuggestionFlight, now, latest time.Time) []connectionSuggestion {
	result := make([]connectionSuggestion, 0)
	var matches []connectionSuggestion
	for _, onward := range candidates {
		out := onward.Flight
		departure := first(out.EstimatedDeparture, out.ScheduledDeparture)
		if departure == nil || !departure.After(now) || departure.After(latest) || connectionDeparted(out) {
			continue
		}
		matches = matches[:0]
		for _, inbound := range candidates {
			in := inbound.Flight
			if in.ID == out.ID || connectionDisrupted(in, out) || !connectionAirportsMatch(in, out) ||
				!canStartConnection(in, out, now) || slices.Contains(inbound.MonitoredOnward, out.ID) {
				continue
			}
			gap := out.ScheduledDeparture.Sub(*in.ScheduledArrival)
			source := "schedule"
			if slices.Contains(inbound.BookedOnward, out.ID) {
				source = "booking"
			} else if gap > core.ScheduleConnectionWindow {
				continue
			}
			airport := in.Destination
			if airport == "" {
				airport = out.Origin
			}
			if airport == "" {
				airport = in.DestinationICAO
			}
			timeZone := out.OriginTimeZone
			if timeZone == "" {
				timeZone = in.DestinationTimeZone
			}
			matches = append(matches, connectionSuggestion{
				Inbound: in.ID, Outbound: out.ID, InboundNumber: in.Number, OutboundNumber: out.Number,
				Airport: airport, TimeZone: timeZone,
				ScheduledArrival: in.ScheduledArrival.UTC(), ScheduledDeparture: out.ScheduledDeparture.UTC(),
				ScheduledMinutes: int(gap / time.Minute), Source: source, departure: *departure,
			})
		}
		slices.SortFunc(matches, compareConnectionSuggestions)
		result = append(result, matches[:min(len(matches), connectionSuggestionsPerOnward)]...)
	}
	slices.SortFunc(result, compareConnectionSuggestions)
	return result[:min(len(result), connectionSuggestionLimit)]
}

func compareConnectionSuggestions(a, b connectionSuggestion) int {
	if bySource := cmp.Compare(a.Source, b.Source); bySource != 0 {
		return bySource
	}
	if byDeparture := a.departure.Compare(b.departure); byDeparture != 0 {
		return byDeparture
	}
	if byGap := cmp.Compare(a.ScheduledDeparture.Sub(a.ScheduledArrival), b.ScheduledDeparture.Sub(b.ScheduledArrival)); byGap != 0 {
		return byGap
	}
	if byInbound := cmp.Compare(a.Inbound, b.Inbound); byInbound != 0 {
		return byInbound
	}
	return cmp.Compare(a.Outbound, b.Outbound)
}
