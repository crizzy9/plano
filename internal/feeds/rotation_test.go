package feeds

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRotationLateExplicitLinkWithoutScheduledArrival(t *testing.T) {
	target := rotationTarget(t, "INBOUND", "")
	api, requests := rotationFixture(t, map[string]rotationReply{
		"INBOUND": {200, `[{"fa_flight_id":"INBOUND","registration":"N1AA","origin":{"code_icao":"KDEN"},"destination":{"code_icao":"KSFO"},"actual_out":"2026-11-05T19:00:00Z","estimated_in":"2026-11-05T23:00:00Z"}]`},
		"N1AA":    {200, `[{"fa_flight_id":"EARLIER","registration":"N1AA","destination":{"code_icao":"KDEN"},"scheduled_out":"2026-11-05T13:00:00Z","scheduled_in":"2026-11-05T16:00:00Z"}]`},
	})
	got, err := api.Rotation(t.Context(), target)
	if err != nil || !slices.Equal(rotationIDs(got), []string{"INBOUND", "EARLIER"}) || !slices.Equal(*requests, []string{"INBOUND", "N1AA"}) {
		t.Fatalf("rotation IDs = %v, error = %v, requests = %v", rotationIDs(got), err, *requests)
	}
}

func TestRotationRetainsKnownRegistrationAcrossMissingTail(t *testing.T) {
	target := rotationTarget(t, "INBOUND", "N1AA")
	api, _ := rotationFixture(t, map[string]rotationReply{
		"INBOUND": {200, `[{"fa_flight_id":"INBOUND","inbound_fa_flight_id":"WRONG","origin":{"code_icao":"KDEN"},"destination":{"code_icao":"KSFO"},"scheduled_out":"2026-11-05T18:00:00Z","scheduled_in":"2026-11-05T21:00:00Z"}]`},
		"WRONG":   {200, `[{"fa_flight_id":"WRONG","registration":"N2AA","destination":{"code_icao":"KDEN"},"scheduled_out":"2026-11-05T15:00:00Z","scheduled_in":"2026-11-05T17:00:00Z"}]`},
		"N1AA":    {200, `[{"fa_flight_id":"EARLIER","registration":"N1AA","destination":{"code_icao":"KDEN"},"scheduled_out":"2026-11-05T13:00:00Z","scheduled_in":"2026-11-05T16:00:00Z"}]`},
	})
	got, err := api.Rotation(t.Context(), target)
	if err != nil || !slices.Equal(rotationIDs(got), []string{"INBOUND", "EARLIER"}) {
		t.Fatalf("rotation IDs = %v, error = %v", rotationIDs(got), err)
	}
}

func TestRotationTailFallbackUsesLatestMovement(t *testing.T) {
	const older = `{"fa_flight_id":"OLDER","registration":"N1AA","destination":{"code_icao":"KSFO"},"scheduled_out":"2026-11-05T15:00:00Z","scheduled_in":"2026-11-05T17:00:00Z","estimated_in":"2026-11-05T23:30:00Z"}`
	const nearer = `{"fa_flight_id":"NEARER","registration":"N1AA","origin":{"code_icao":"KDEN"},"destination":{"code_icao":"KSFO"},"scheduled_out":"2026-11-05T18:00:00Z","scheduled_in":"2026-11-05T21:00:00Z"}`
	const intervening = `{"fa_flight_id":"INTERVENING","registration":"N1AA","destination":{"code_icao":"KOAK"},"scheduled_out":"2026-11-05T17:00:00Z","scheduled_in":"2026-11-05T21:30:00Z","actual_out":"2026-11-05T19:00:00Z","actual_in":"2026-11-05T21:30:00Z"}`
	const lateIntervening = `{"fa_flight_id":"LATE","registration":"N1AA","destination":{"code_icao":"KOAK"},"scheduled_out":"2026-11-05T17:00:00Z","scheduled_in":"2026-11-05T23:30:00Z","actual_off":"2026-11-05T19:00:00Z"}`
	for _, test := range []struct {
		name string
		rows []string
		want []string
	}{
		{"older late ETA", []string{nearer, older}, []string{"NEARER"}},
		{"intervening move to a different airport", []string{older, intervening, nearer}, nil},
		{"intervening move arriving after the outbound schedule", []string{nearer, older, lateIntervening}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, requests := rotationFixture(t, map[string]rotationReply{
				"N1AA": {200, "[" + strings.Join(test.rows, ",") + "]"},
			})
			got, err := api.Rotation(t.Context(), rotationTarget(t, "", "N1AA"))
			if err != nil || got == nil || !slices.Equal(rotationIDs(got), test.want) || !slices.Equal(*requests, []string{"N1AA"}) {
				t.Fatalf("rotation IDs = %v, error = %v, requests = %v", rotationIDs(got), err, *requests)
			}
		})
	}
}

func TestRotationRejectsInboundLandingAfterActualOutbound(t *testing.T) {
	target := rotationTarget(t, "INBOUND", "")
	takeoff := instant(t, "2026-11-05T22:15:00Z")
	target.ActualTakeoff = &takeoff
	api, _ := rotationFixture(t, map[string]rotationReply{
		"INBOUND": {200, `[{"fa_flight_id":"INBOUND","destination":{"code_icao":"KSFO"},"scheduled_out":"2026-11-05T18:00:00Z","scheduled_in":"2026-11-05T20:00:00Z","estimated_in":"2026-11-05T21:00:00Z","actual_on":"2026-11-05T22:30:00Z"}]`},
	})
	got, err := api.Rotation(t.Context(), target)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("rotation IDs = %v, error = %v", rotationIDs(got), err)
	}
}

func TestRotationRejectedExplicitMoveStillBlocksOlderTailMatch(t *testing.T) {
	const explicit = `{"fa_flight_id":"OTHER","registration":"N1AA","destination":{"code_icao":"KOAK"},"scheduled_out":"2026-11-05T17:00:00Z","scheduled_in":"2026-11-05T23:30:00Z"}`
	observed := strings.TrimSuffix(explicit, "}") + `,"actual_off":"2026-11-05T19:00:00Z"}`
	const older = `{"fa_flight_id":"OLDER","registration":"N1AA","destination":{"code_icao":"KSFO"},"scheduled_out":"2026-11-05T18:00:00Z","scheduled_in":"2026-11-05T21:00:00Z"}`
	for _, test := range []struct {
		name string
		link string
		rows string
	}{
		{"movement found in explicit lookup", observed, older},
		{"movement found in registration history", explicit, older + "," + observed},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, requests := rotationFixture(t, map[string]rotationReply{
				"OTHER": {200, "[" + test.link + "]"},
				"N1AA":  {200, "[" + test.rows + "]"},
			})
			got, err := api.Rotation(t.Context(), rotationTarget(t, "OTHER", "N1AA"))
			if err != nil || got == nil || len(got) != 0 || !slices.Equal(*requests, []string{"OTHER", "N1AA"}) {
				t.Fatalf("rotation IDs = %v, error = %v, requests = %v", rotationIDs(got), err, *requests)
			}
		})
	}
}

func TestRotationOutageKeepsVerifiedPrefixAndDoesNotRetry(t *testing.T) {
	t.Run("rate limited before any verification", func(t *testing.T) {
		api, requests := rotationFixture(t, map[string]rotationReply{"LIMITED": {429, ""}})
		got, err := api.Rotation(t.Context(), rotationTarget(t, "LIMITED", "N1AA"))
		if got != nil || !errors.Is(err, ErrUnavailable) || !slices.Equal(*requests, []string{"LIMITED"}) {
			t.Fatalf("rotation IDs = %v, error = %v, requests = %v", rotationIDs(got), err, *requests)
		}
	})
	t.Run("cached registration rows can extend the verified prefix", func(t *testing.T) {
		api, requests := rotationFixture(t, map[string]rotationReply{
			"N1AA":    {200, `[{"fa_flight_id":"NEARER","inbound_fa_flight_id":"LIMITED","registration":"N1AA","origin":{"code_icao":"KDEN"},"destination":{"code_icao":"KSFO"},"scheduled_out":"2026-11-05T18:00:00Z","scheduled_in":"2026-11-05T21:00:00Z"},{"fa_flight_id":"EARLIER","inbound_fa_flight_id":"NO_MORE_REQUESTS","registration":"N1AA","destination":{"code_icao":"KDEN"},"scheduled_out":"2026-11-05T13:00:00Z","scheduled_in":"2026-11-05T16:00:00Z"}]`},
			"LIMITED": {429, ""},
		})
		got, err := api.Rotation(t.Context(), rotationTarget(t, "", "N1AA"))
		if !errors.Is(err, ErrUnavailable) || !slices.Equal(rotationIDs(got), []string{"NEARER", "EARLIER"}) || !slices.Equal(*requests, []string{"N1AA", "LIMITED"}) {
			t.Fatalf("rotation IDs = %v, error = %v, requests = %v", rotationIDs(got), err, *requests)
		}
	})
}

type rotationReply struct {
	status  int
	flights string
}

func rotationFixture(t *testing.T, replies map[string]rotationReply) (*AeroAPI, *[]string) {
	t.Helper()
	var requests []string
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		id := strings.TrimPrefix(r.URL.Path, "/aeroapi/flights/")
		requests = append(requests, id)
		reply, ok := replies[id]
		if !ok {
			t.Fatalf("unexpected provider request %s", r.URL.Path)
		}
		return response(reply.status, `{"flights":`+reply.flights+`}`)
	})}
	api := NewAeroAPI("fixture-key", client)
	api.now = func() time.Time { return instant(t, "2026-11-05T20:00:00Z") }
	return api, &requests
}

func rotationTarget(t *testing.T, inboundID, tail string) Flight {
	t.Helper()
	departure := instant(t, "2026-11-05T22:00:00Z")
	return Flight{ID: "OUTBOUND", InboundID: inboundID, TailNumber: tail, OriginICAO: "KSFO", ScheduledDeparture: &departure}
}

func rotationIDs(flights []Flight) []string {
	ids := make([]string, 0, len(flights))
	for _, f := range flights {
		ids = append(ids, f.ID)
	}
	return ids
}
