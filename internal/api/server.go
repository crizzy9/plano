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
)

type Server struct {
	Store          *core.Store
	Source         feeds.FlightSource
	PublicURL      string
	EnrollmentCode string
	Demo           bool
	Logger         *slog.Logger
}

type ownerHandler func(http.ResponseWriter, *http.Request, string) error

var (
	flightID     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	flightNumber = regexp.MustCompile(`^[A-Z0-9]{2,3}[0-9]{1,5}[A-Z]?$`)
	seatPattern  = regexp.MustCompile(`^[0-9]{1,3}[A-Z]{1,2}$`)
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
	mux.HandleFunc("GET /v1/tracked", s.auth(s.tracked))
	mux.HandleFunc("POST /v1/tracked", s.auth(s.track))
	mux.HandleFunc("DELETE /v1/tracked/{id}", s.auth(s.untrack))
	mux.HandleFunc("POST /v1/devices", s.auth(s.device))
	mux.HandleFunc("DELETE /v1/devices", s.auth(s.device))
	mux.HandleFunc("POST /v1/shares", s.auth(s.share))
	mux.HandleFunc("GET /share/{token}", s.shared)
	mux.HandleFunc("GET /v1/passport", s.auth(s.passport))
	mux.HandleFunc("GET /v1/connections", s.auth(s.connection))
	mux.HandleFunc("POST /v1/import", s.auth(s.importFlights))
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

func (s Server) details(w http.ResponseWriter, r *http.Request, _ string) error {
	id := r.PathValue("id")
	if !flightID.MatchString(id) {
		return errInvalid
	}
	d, err := s.Store.Details(r.Context(), id)
	if err != nil {
		return err
	}
	respond(w, 200, d)
	return nil
}

func (s Server) tracked(w http.ResponseWriter, r *http.Request, owner string) error {
	f, err := s.Store.Tracked(r.Context(), owner)
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
	if d.RequestUpdateToken && d.Kind != "live_start" || len(d.Token) < 20 || len(d.Token) > 4096 || strings.ContainsAny(d.Token, " \t\r\n") {
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
	if err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}
