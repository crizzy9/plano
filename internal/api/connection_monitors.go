package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) connectionMonitors(w http.ResponseWriter, r *http.Request, owner string) error {
	ctx := r.Context()
	if r.Method == http.MethodGet {
		monitors, err := s.Store.ConnectionMonitors(ctx, owner)
		if err != nil {
			return err
		}
		respond(w, http.StatusOK, map[string]any{"monitors": monitors})
		return nil
	}
	inID, outID, err := connectionIDs(r)
	if err != nil {
		return err
	}
	if r.Method == http.MethodDelete {
		if err := s.Store.UnmonitorConnection(ctx, owner, inID, outID); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	err = s.Store.MonitorConnection(ctx, owner, inID, outID, time.Now(), s.assessNewConnection)
	if errors.Is(err, core.ErrInvalidConnection) || errors.Is(err, core.ErrNotFound) {
		respond(w, http.StatusBadRequest, map[string]string{"error": "connection cannot be monitored"})
		return nil
	}
	if err != nil {
		return err
	}
	respond(w, http.StatusOK, map[string]bool{"monitoring": true})
	return nil
}

func (s Server) assessNewConnection(ctx context.Context, in, out feeds.Flight, now time.Time, predictGate core.ConnectionGatePredictor) (core.ConnectionAssessment, error) {
	if !canStartConnection(in, out, now) {
		return core.ConnectionAssessment{}, nil
	}
	return s.evaluateConnection(ctx, in, out, now, predictGate)
}

func (s Server) evaluateConnection(ctx context.Context, in, out feeds.Flight, now time.Time, predictGate core.ConnectionGatePredictor) (core.ConnectionAssessment, error) {
	// A diversion can change the inbound destination and invalidate the route.
	// A known disruption still ends this previously validated connection.
	if connectionDisrupted(in, out) {
		return missedConnectionAssessment(in, out, now), nil
	}
	if connectionAirportsKnown(in, out) && !connectionAirportsMatch(in, out) {
		return core.ConnectionAssessment{}, nil
	}
	_, assessment, err := s.assessConnection(ctx, in, out, now, predictGate)
	if errors.Is(err, errInvalid) {
		return core.ConnectionAssessment{Incomplete: !connectionDeparted(out)}, nil
	}
	return assessment, err
}

func (s Server) RunConnectionMonitors(ctx context.Context) {
	s.Store.RunConnectionMonitors(ctx, s.evaluateConnection, s.Logger)
}
