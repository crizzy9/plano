// Package api exposes Plano's installation-scoped HTTP interface.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/historicalrisk"
)

type Server struct {
	Store                *core.Store
	Source               feeds.FlightSource
	PublicURL            string
	EnrollmentCode       string
	BookingKey           []byte
	ReservationSource    feeds.ReservationSource
	TripItSync           *TripItSync
	Weather              *core.WeatherCache
	ConnectionRules      *ConnectionRules
	HistoricalRisk       *historicalrisk.Model
	Demo                 bool
	Logger               *slog.Logger
	AeroCallbackKey      []byte
	AdvisoryIngressKey   []byte
	FlightObservationKey []byte
}

type ownerHandler func(http.ResponseWriter, *http.Request, string) error

var (
	flightID     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	flightNumber = regexp.MustCompile(`^[A-Z0-9]{2,3}[0-9]{1,5}[A-Z]?$`)
	seatPattern  = regexp.MustCompile(`^[0-9]{1,3}[A-Z]{1,2}$`)
	uuidV4       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	ianaZoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`)
	errInvalid   = errors.New("invalid request")
)

func (s Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.Health(r.Context()); err != nil {
			s.failure(w, err)
			return
		}
		mode := "live"
		if s.Demo {
			mode = "demo"
		}
		respond(w, 200, map[string]string{"status": "ok", "mode": mode})
	})
	mux.HandleFunc("POST /v1/installations", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			EnrollmentCode string `json:"enrollmentCode"`
		}
		if err := parse(w, r, &input, 1024); err != nil {
			s.failure(w, err)
			return
		}
		if s.EnrollmentCode != "" {
			want, got := sha256.Sum256([]byte(s.EnrollmentCode)), sha256.Sum256([]byte(input.EnrollmentCode))
			if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
				respond(w, 403, map[string]string{"error": "valid enrollment code required"})
				return
			}
		}
		token, err := s.Store.CreateInstallation(r.Context())
		if err != nil {
			s.failure(w, err)
			return
		}
		respond(w, 201, map[string]string{"token": token})
	})
	mux.HandleFunc("GET /v1/flights", s.auth(s.search))
	mux.HandleFunc("GET /v1/flights/{id}", s.auth(s.details))
	mux.HandleFunc("GET /v1/flights/{id}/radar", s.auth(s.radar))
	mux.HandleFunc("GET /v1/tracked", s.auth(s.tracked))
	mux.HandleFunc("POST /v1/tracked", s.auth(s.track))
	mux.HandleFunc("DELETE /v1/tracked/{id}", s.auth(s.untrack))
	mux.HandleFunc("POST /v1/devices", s.auth(s.device))
	mux.HandleFunc("DELETE /v1/devices", s.auth(s.device))
	mux.HandleFunc("POST /v1/shares", s.auth(s.share))
	mux.HandleFunc("GET /v1/shares", s.auth(s.pickupLinks))
	mux.HandleFunc("DELETE /v1/shares/{id}", s.auth(s.revokePickupLink))
	mux.HandleFunc("GET /share/{token}", s.shared)
	mux.HandleFunc("GET /v1/passport", s.auth(s.passport))
	mux.HandleFunc("POST /v1/passport/import", s.auth(s.importPassportArchive))
	mux.HandleFunc("POST /v1/passport/manual", s.auth(s.saveManualPassport))
	mux.HandleFunc("PUT /v1/passport/manual/{id}", s.auth(s.saveManualPassport))
	mux.HandleFunc("DELETE /v1/passport/archive/{id}", s.auth(s.deletePassportArchive))
	mux.HandleFunc("GET /v1/connections", s.auth(s.connection))
	mux.HandleFunc("GET /v1/connection-suggestions", s.auth(s.connectionSuggestions))
	mux.HandleFunc("GET /v1/connection-monitors", s.auth(s.connectionMonitors))
	mux.HandleFunc("PUT /v1/connection-monitors", s.auth(s.connectionMonitors))
	mux.HandleFunc("DELETE /v1/connection-monitors", s.auth(s.connectionMonitors))
	mux.HandleFunc("POST /v1/import", s.auth(s.importFlights))
	mux.HandleFunc("POST /v1/import/calendar-sync", s.auth(s.syncCalendar))
	mux.HandleFunc("POST /v1/import/tripit", s.auth(s.importTripIt))
	mux.HandleFunc("GET /v1/import/tripit/subscription", s.auth(s.tripItSubscription))
	mux.HandleFunc("PUT /v1/import/tripit/subscription", s.auth(s.tripItSubscription))
	mux.HandleFunc("DELETE /v1/import/tripit/subscription", s.auth(s.tripItSubscription))
	mux.HandleFunc("POST /v1/import/email-relay", s.auth(s.emailRelay))
	mux.HandleFunc("DELETE /v1/import/email-relay", s.auth(s.emailRelay))
	mux.HandleFunc("POST /v1/webhooks/email", s.emailWebhook)
	mux.HandleFunc("POST /v1/bookings/lookup", s.auth(s.booking))
	mux.HandleFunc("POST /v1/bookings/resolve", s.auth(s.resolveBooking))
	if len(s.AeroCallbackKey) != 0 {
		mux.HandleFunc("POST /v1/webhooks/aeroapi", s.aeroCallback)
	}
	if len(s.AdvisoryIngressKey) != 0 {
		mux.HandleFunc("POST /v1/webhooks/advisory", s.advisoryCallback)
	}
	if !s.Demo && len(s.FlightObservationKey) != 0 {
		mux.HandleFunc("POST /v1/webhooks/flight-observation", s.flightObservationCallback)
	}
	s.registerFriendRoutes(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

func (s Server) auth(handler ownerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || len(token) < 20 || len(token) > 128 {
			respond(w, 401, map[string]string{"error": "valid installation token required"})
			return
		}
		owner, err := s.Store.Installation(r.Context(), token)
		if errors.Is(err, core.ErrNotFound) {
			respond(w, 401, map[string]string{"error": "valid installation token required"})
			return
		}
		if err == nil {
			err = handler(w, r, owner)
		}
		if err != nil {
			s.failure(w, err)
		}
	}
}

func (s Server) failure(w http.ResponseWriter, err error) {
	status, message := 500, "internal error"
	switch {
	case errors.Is(err, errInvalid), errors.Is(err, feeds.ErrInvalidQuery):
		status, message = 400, "invalid request"
	case errors.Is(err, core.ErrNotFound), errors.Is(err, feeds.ErrNotFound):
		status, message = 404, "record not found"
	case errors.Is(err, feeds.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		status, message = 503, "aviation source unavailable"
	default:
		s.Logger.Error("api request failed", "error", err)
	}
	respond(w, status, map[string]string{"error": message})
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func parse(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return errInvalid
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errInvalid
	}
	return nil
}

func (s Server) lookup(ctx context.Context, number, date string) ([]feeds.Flight, error) {
	number = strings.ToUpper(strings.Join(strings.Fields(number), ""))
	day, err := time.Parse(time.DateOnly, date)
	if err != nil || !flightNumber.MatchString(number) {
		return nil, errInvalid
	}
	flights, err := s.Source.Search(ctx, number, day)
	if err != nil {
		return nil, err
	}
	if flights == nil {
		flights = make([]feeds.Flight, 0)
	}
	for _, f := range flights {
		if err := s.Store.Observe(ctx, f, nil, nil, nil); err != nil {
			return nil, err
		}
	}
	return flights, nil
}

func (s Server) search(w http.ResponseWriter, r *http.Request, _ string) error {
	f, err := s.lookup(r.Context(), r.URL.Query().Get("number"), r.URL.Query().Get("date"))
	if err != nil {
		return err
	}
	respond(w, 200, map[string]any{"flights": f})
	return nil
}

func (s Server) details(w http.ResponseWriter, r *http.Request, owner string) error {
	id := r.PathValue("id")
	rotationOf := r.URL.Query().Get("rotationOf")
	if !flightID.MatchString(id) || rotationOf != "" && (!flightID.MatchString(rotationOf) || rotationOf == id) {
		return errInvalid
	}
	ctx := r.Context()
	d, err := s.Store.DetailsForInstallation(ctx, owner, id)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	var rotationSnapshot bool
	if rotationOf != "" && !d.Tracked {
		rotation, rotationErr := s.rotationDetails(ctx, owner, rotationOf, id, time.Now())
		if rotationErr == nil {
			d, err, rotationSnapshot = rotation, nil, true
		} else if !errors.Is(rotationErr, core.ErrNotFound) {
			return rotationErr
		}
	}
	if err != nil {
		return err
	}
	var weather []core.AirportWeather
	var notices []core.FlightNotice
	now := time.Now()
	if d.Tracked {
		weather = s.Weather.ForFlight(d.Flight, now)
		notices, err = s.Store.FlightNotices(r.Context(), d.Flight, now)
		if err != nil {
			notices = nil
			if r.Context().Err() == nil && s.Logger != nil {
				s.Logger.Warn("flight notice context unavailable")
			}
		}
	}
	var historicalRisk *historicalrisk.Estimate
	if !rotationSnapshot {
		historicalRisk = s.historicalDepartureRisk(d.Flight, now)
	}
	respond(w, 200, struct {
		core.Details
		Weather                 []core.AirportWeather    `json:"weather,omitempty"`
		Notices                 []core.FlightNotice      `json:"notices,omitempty"`
		RotationSnapshot        bool                     `json:"rotationSnapshot,omitempty"`
		HistoricalDepartureRisk *historicalrisk.Estimate `json:"historicalDepartureRisk,omitempty"`
	}{d, weather, notices, rotationSnapshot, historicalRisk})
	return nil
}

func (s Server) historicalDepartureRisk(flight feeds.Flight, now time.Time) *historicalrisk.Estimate {
	if s.HistoricalRisk == nil || s.Demo || flight.ScheduledDeparture == nil ||
		(flight.Status != "scheduled" && flight.Status != "boarding") ||
		first(flight.ActualDeparture, flight.ActualTakeoff, flight.ActualLanding, flight.ActualArrival) != nil ||
		!flight.ScheduledDeparture.After(now) {
		return nil
	}
	zone := flight.OriginTimeZone
	if len(zone) > 128 || !ianaZoneName.MatchString(zone) || zone == "Local" || zone == "localtime" || zone == "posixrules" ||
		strings.HasPrefix(zone, "posix/") || strings.HasPrefix(zone, "right/") {
		return nil
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil
	}
	estimate, ok := s.HistoricalRisk.Estimate(flight.Origin, flight.Destination, flight.ScheduledDeparture.In(location), now)
	if !ok {
		return nil
	}
	return &estimate
}

func (s Server) tracked(w http.ResponseWriter, r *http.Request, owner string) error {
	ids, filtered := r.URL.Query()["flightId"]
	var f []feeds.Flight
	var err error
	if filtered {
		if len(ids) < 1 || len(ids) > 2 || !flightID.MatchString(ids[0]) {
			return errInvalid
		}
		var secondID string
		if len(ids) == 2 {
			secondID = ids[1]
			if !flightID.MatchString(secondID) {
				return errInvalid
			}
		}
		f, err = s.Store.TrackedFlights(r.Context(), owner, ids[0], secondID)
	} else {
		f, err = s.Store.Tracked(r.Context(), owner)
	}
	if err != nil {
		return err
	}
	respond(w, 200, map[string]any{"flights": f})
	return nil
}

func (s Server) track(w http.ResponseWriter, r *http.Request, owner string) error {
	var input struct {
		FlightID string `json:"flightId"`
		Seat     string `json:"seat"`
	}
	if err := parse(w, r, &input, 4096); err != nil {
		return err
	}
	seat := strings.ToUpper(strings.TrimSpace(input.Seat))
	if !flightID.MatchString(input.FlightID) || seat != "" && !seatPattern.MatchString(seat) {
		return errInvalid
	}
	if err := s.Store.Track(r.Context(), owner, input.FlightID, seat); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s Server) untrack(w http.ResponseWriter, r *http.Request, owner string) error {
	id := r.PathValue("id")
	if !flightID.MatchString(id) {
		return errInvalid
	}
	if err := s.Store.Untrack(r.Context(), owner, id); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s Server) device(w http.ResponseWriter, r *http.Request, owner string) error {
	var d core.Device
	if r.Method != http.MethodDelete || r.ContentLength != 0 {
		if err := parse(w, r, &d, 8192); err != nil {
			return err
		}
	}
	if r.Method == http.MethodDelete && d == (core.Device{}) {
		if err := s.Store.RemoveDevices(r.Context(), owner); err != nil {
			return err
		}
		w.WriteHeader(204)
		return nil
	}
	if r.Method == http.MethodDelete && d.LiveRevision > 0 && d == (core.Device{Platform: "ios", LiveRevision: d.LiveRevision}) {
		if err := s.Store.RemoveIOSDevicesThroughRevision(r.Context(), owner, d.LiveRevision); err != nil {
			return err
		}
		w.WriteHeader(204)
		return nil
	}
	if (d.LiveSlot != "" || d.LiveRevision != 0) && r.Method != http.MethodPost {
		return errInvalid
	}
	if d.Binding != "" {
		androidAlert := d.Platform == "android" && d.Kind == "alert"
		iosActivity := r.Method == http.MethodPost && d.Platform == "ios" && d.LiveSlot != "" &&
			(d.Kind == "live_start" || d.Kind == "live_update")
		if (!androidAlert && !iosActivity) || !uuidV4.MatchString(d.Binding) {
			return errInvalid
		}
	}
	bindingRevocation := r.Method == http.MethodDelete && d.Platform == "android" && d.Token == "" && d.Binding != ""
	if d.RequestUpdateToken && d.Kind != "live_start" || !bindingRevocation && (len(d.Token) < 20 || len(d.Token) > 4096 || strings.ContainsAny(d.Token, " \t\r\n")) {
		return errInvalid
	}
	switch d.Kind {
	case "alert":
		if d.Platform != "ios" && d.Platform != "android" || d.FlightID != "" {
			return errInvalid
		}
	case "live_start":
		if d.Platform != "ios" || d.FlightID != "" {
			return errInvalid
		}
	case "live_update":
		if d.Platform != "ios" || !flightID.MatchString(d.FlightID) {
			return errInvalid
		}
	default:
		return errInvalid
	}
	var err error
	if r.Method == http.MethodDelete {
		err = s.Store.RemoveDevice(r.Context(), owner, d)
	} else {
		err = s.Store.Register(r.Context(), owner, d, time.Now())
	}
	if errors.Is(err, core.ErrInvalidLiveSlot) {
		return errInvalid
	}
	if err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}
