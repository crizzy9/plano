package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

const (
	inboundTakeoff           = "inbound_takeoff"
	inboundLanding           = "inbound_landing"
	inboundMilestoneLifetime = 15 * time.Minute
)

func inboundMilestoneTime(kind string, onward, leg feeds.Flight, now time.Time) *time.Time {
	scheduled := onward.ScheduledDeparture
	if scheduled == nil || now.Before(scheduled.Add(-25*time.Hour)) || !now.Before(scheduled.Add(6*time.Hour)) ||
		!flightAlertRelevant("delay_prediction", onward) || !isExactAeroInbound(onward, leg) || !connectsAeroInbound(onward, leg) ||
		leg.Status == "cancelled" || leg.Status == "diverted" {
		return nil
	}
	var at *time.Time
	switch kind {
	case inboundTakeoff:
		if leg.ActualLanding != nil || leg.ActualArrival != nil || leg.Status == "landed" || leg.Status == "arrived" {
			return nil
		}
		at = leg.ActualTakeoff
	case inboundLanding:
		at = leg.ActualLanding
		if at != nil && leg.ActualTakeoff != nil && at.Before(*leg.ActualTakeoff) {
			return nil
		}
	}
	if at == nil || at.After(now) || at.After(leg.ObservedAt) || !now.Before(at.Add(inboundMilestoneLifetime)) {
		return nil
	}
	return at
}

// A new milestone must follow an earlier observation of the same linked leg.
// A first observation of an aircraft already in motion supplies history only.
func saveInboundMilestones(ctx context.Context, tx *sql.Tx, onward feeds.Flight, before, after []feeds.Flight, now time.Time) error {
	if len(before) == 0 || len(after) == 0 {
		return nil
	}
	prior, leg := before[0], after[0]
	if !isExactAeroInbound(onward, prior) || !connectsAeroInbound(onward, prior) || !sameInboundHistory(prior, leg) ||
		!leg.ObservedAt.After(prior.ObservedAt) {
		return nil
	}
	kind, title := inboundLanding, "Inbound aircraft landed"
	if leg.ActualLanding == nil {
		kind, title = inboundTakeoff, "Inbound aircraft took off"
	}
	if kind == inboundLanding && prior.ActualLanding != nil ||
		kind == inboundTakeoff && (prior.ActualTakeoff != nil || prior.ActualLanding != nil || prior.ActualArrival != nil || prior.Status == "landed" || prior.Status == "arrived") {
		return nil
	}
	at := inboundMilestoneTime(kind, onward, leg, now)
	if at == nil || leg.Number == "" || onward.Number == "" {
		return nil
	}
	validUntil := at.Add(inboundMilestoneLifetime)
	event := Event{ID: hash(onward.ID + "\x00" + kind + "\x00" + leg.ID), Kind: kind, Title: title,
		Body: fmt.Sprintf("%s · inbound %s", onward.Number, leg.Number), At: at.UTC(), validUntil: &validUntil, inboundLegID: leg.ID}
	return saveEvent(ctx, tx, onward.ID, event, true)
}

// A queued or retried alert is eligible only while its onward flight and
// exact inbound movement still agree at the transport boundary.
func (s *Store) inboundMilestoneCurrent(ctx context.Context, job deliveryJob, kind string, now time.Time) (bool, error) {
	var rawFlight, rawRotation []byte
	err := s.db.QueryRowContext(ctx, `SELECT f.snapshot,f.inbound FROM outbox o JOIN flights f ON f.id=o.flight_id
WHERE o.id=? AND o.device_id=? AND o.lease_until=? AND o.done=0
AND EXISTS(SELECT 1 FROM tracked t WHERE t.flight_id=f.id AND t.owner=?)`,
		job.id, job.deviceID, job.lease, job.owner).Scan(&rawFlight, &rawRotation)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var onward feeds.Flight
	if err := json.Unmarshal(rawFlight, &onward); err != nil {
		return false, err
	}
	var rotation []feeds.Flight
	if err := json.Unmarshal(rawRotation, &rotation); err != nil {
		return false, err
	}
	return len(rotation) != 0 && rotation[0].ID == job.message.InboundLegID && inboundMilestoneTime(kind, onward, rotation[0], now) != nil, nil
}
