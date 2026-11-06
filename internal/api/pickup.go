package api

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) share(w http.ResponseWriter, r *http.Request, owner string) error {
	var input struct {
		FlightID string `json:"flightId"`
	}
	if err := parse(w, r, &input, 4096); err != nil {
		return err
	}
	if !flightID.MatchString(input.FlightID) {
		return errInvalid
	}
	now := time.Now().UTC()
	link, token, err := s.Store.CreatePickupLink(r.Context(), owner, input.FlightID, now, now.Add(72*time.Hour))
	if err != nil {
		return err
	}
	respond(w, http.StatusCreated, struct {
		core.PickupLink
		URL string `json:"url"`
	}{link, s.PublicURL + "/share/" + token})
	return nil
}

func (s Server) pickupLinks(w http.ResponseWriter, r *http.Request, owner string) error {
	id := r.URL.Query().Get("flightId")
	if !flightID.MatchString(id) {
		return errInvalid
	}
	links, err := s.Store.PickupLinks(r.Context(), owner, id, time.Now())
	if err != nil {
		return err
	}
	respond(w, 200, map[string]any{"links": links})
	return nil
}

var pickupID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s Server) revokePickupLink(w http.ResponseWriter, r *http.Request, owner string) error {
	id := r.PathValue("id")
	if !pickupID.MatchString(id) {
		return errInvalid
	}
	if err := s.Store.RevokePickupLink(r.Context(), owner, id); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

const pickupStyle = `
:root{color-scheme:light dark;--page:#f4f7f3;--card:#fff;--ink:#142b24;--muted:#49625a;--line:#d9e4dd;--accent:#167653;--water:#e8f1ef;--land:#c6dbd0}
@media(prefers-color-scheme:dark){:root{--page:#111b19;--card:#182724;--ink:#eef7f2;--muted:#b0c7bc;--line:#38524a;--accent:#7bd5ab;--water:#172a29;--land:#304a41}}
*{box-sizing:border-box}body{margin:0;background:var(--page);color:var(--ink);font:1rem/1.5 system-ui,sans-serif}
main{max-width:40rem;margin:0 auto;padding:clamp(1.25rem,5vw,3rem)}header{margin-bottom:1.4rem}.brand{color:var(--accent);font-size:1.15rem;font-weight:750}h1{font-size:clamp(1.8rem,8vw,3.2rem);line-height:1.12;margin:.5rem 0}.muted{color:var(--muted)}
section{background:var(--card);border:1px solid var(--line);border-radius:1rem;padding:1.35rem}h2{font-size:1rem;margin:0;color:var(--muted);font-weight:600}.arrival{font-weight:750;font-size:clamp(1.15rem,5.4vw,1.5rem);font-variant-numeric:tabular-nums;text-wrap:balance;margin:.2rem 0}.status{font-weight:650;color:var(--accent);margin:0}
dl{margin:1rem 0 0;display:grid;grid-template-columns:repeat(auto-fit,minmax(7rem,1fr));gap:1rem}dt{font-size:.9rem;color:var(--muted)}dd{margin:0;font-weight:650}footer{margin-top:1rem;font-size:.9rem}
.route-map{margin:1.4rem 0 0}.route-map h2{color:var(--ink);font-weight:700}.route-map p{font-size:.9rem;margin:.2rem 0 .7rem;font-variant-numeric:tabular-nums}
.route-map svg{display:block;width:100%;height:auto;overflow:hidden;border:1px solid var(--line);border-radius:.85rem;background:var(--water)}
.route-land{fill:var(--land)}.route-track{fill:none;stroke:var(--accent);stroke-width:2.4;stroke-linecap:round;stroke-linejoin:round}.route-fix{fill:var(--accent)}.route-latest{fill:var(--ink);stroke:var(--water);stroke-width:1.7}
.map-credit{display:block;text-align:right;margin-top:.3rem;font-size:.75rem}
`

var pickupTemplate = template.Must(template.New("pickup").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex,nofollow">{{if .Refresh}}<meta http-equiv="refresh" content="30">{{end}}<title>{{.Flight.Number}} pickup · Plano</title><style>` + pickupStyle + `</style></head>
<body><main><header><div class="brand">Plano</div><h1>{{.Flight.Origin}} to {{.Flight.Destination}}</h1><p class="muted">{{if .Notice}}Original route for{{else}}Pickup for{{end}} {{.Flight.Number}}</p></header>
<section aria-label="Arrival details"><p class="status">{{.Status}}</p>{{if .Notice}}<p>{{.Notice}}</p>{{end}}{{if .Arrival}}<h2>{{.ArrivalLabel}}</h2><p class="arrival">{{.Arrival}}</p>{{end}}
{{if .Scheduled}}<p class="muted">Scheduled {{.Scheduled}}</p>{{end}}<dl>
{{with .Flight.ArrivalTerminal}}<div><dt>Terminal</dt><dd>{{.}}</dd></div>{{end}}{{with .Flight.ArrivalGate}}<div><dt>Gate</dt><dd>{{.}}</dd></div>{{end}}{{with .Flight.Baggage}}<div><dt>Baggage carousel</dt><dd>{{.}}</dd></div>{{end}}
</dl></section>{{with .Map}}<figure class="route-map" aria-labelledby="route-title"><figcaption><h2 id="route-title">{{if .Path}}Reported flight path{{else}}Reported flight position{{end}}</h2>
<p class="muted">Latest recorded position at {{.ObservedAt.UTC.Format "15:04 UTC on 02 Jan 2006"}}.</p></figcaption>
<svg viewBox="0 0 360 180" role="img" aria-label="Map of recorded flight positions with the latest position marked">
<defs><path id="pickup-land" d="{{.Coastline}}"/></defs><g class="route-land" transform="{{.LandTransform}}">{{range .Worlds}}<use href="#pickup-land" x="{{.}}"/>{{end}}</g>
{{if .Path}}<path class="route-track" d="{{.Path}}"/>{{end}}{{range .Dots}}<circle class="route-fix" cx="{{.X}}" cy="{{.Y}}" r="2.3"/>{{end}}
<circle class="route-latest" cx="{{.Latest.X}}" cy="{{.Latest.Y}}" r="4.8"/></svg><small class="map-credit muted">{{if eq $.Flight.Source "plano_demo"}}Synthetic positions{{else}}Positions from FlightAware{{end}} · Map data from Natural Earth</small></figure>{{end}}
<footer class="muted">Flight status observed {{.Flight.ObservedAt.UTC.Format "15:04 UTC on 02 Jan 2006"}}.{{if .Stale}} Status may be out of date.{{end}}{{if .Demo}} Synthetic demo flight.{{end}}</footer></main></body></html>`))

func (s Server) shared(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if len(token) < 20 || len(token) > 128 {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	flight, positions, err := s.Store.SharedPickup(r.Context(), token, now)
	if err != nil {
		s.failure(w, err)
		return
	}
	view := pickupView(flight, now, s.Demo || flight.Source == "plano_demo")
	view.Map = pickupMap(positions)
	checksum := sha256.Sum256([]byte(pickupStyle))
	w.Header().Set("Content-Security-Policy", fmt.Sprintf("default-src 'none'; style-src 'sha256-%s'; base-uri 'none'; frame-ancestors 'none'", base64.StdEncoding.EncodeToString(checksum[:])))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pickupTemplate.Execute(w, view); err != nil {
		s.Logger.Warn("shared flight rendering stopped")
	}
}

type pickupPage struct {
	Flight                                           feeds.Flight
	Status, ArrivalLabel, Arrival, Scheduled, Notice string
	Demo, Refresh, Stale                             bool
	Map                                              *pickupTrackMap
}

func pickupView(f feeds.Flight, now time.Time, demo bool) pickupPage {
	view := pickupPage{Flight: f, Status: strings.ReplaceAll(f.Status, "_", " "), Demo: demo}
	if title, ok := map[string]string{"scheduled": "Scheduled", "boarding": "Boarding", "taxi_out": "Taxiing for takeoff", "airborne": "In flight", "landed": "Landed · taxiing to gate", "arrived": "At the gate", "cancelled": "Cancelled", "diverted": "Diverted"}[f.Status]; ok {
		view.Status = title
	}
	view.Refresh = f.Status != "arrived" && f.Status != "cancelled" && f.ActualArrival == nil
	view.Stale = view.Refresh && now.Sub(f.ObservedAt) > 15*time.Minute
	location := time.UTC
	if f.DestinationTimeZone != "" {
		if known, err := time.LoadLocation(f.DestinationTimeZone); err == nil {
			location = known
		}
	}
	format := func(at *time.Time) string { return at.In(location).Format("15:04 MST on 02 Jan 2006") }
	if f.Status == "cancelled" || f.Status == "diverted" {
		view.Flight.ArrivalTerminal, view.Flight.ArrivalGate, view.Flight.Baggage = "", "", ""
		if f.Status == "diverted" {
			view.Notice = "Check with the airline for the pickup airport."
		}
		return view
	}
	if f.ActualArrival != nil {
		view.ArrivalLabel, view.Arrival = "Actual gate arrival", format(f.ActualArrival)
	} else if f.EstimatedArrival != nil {
		view.ArrivalLabel, view.Arrival = "Estimated gate arrival", format(f.EstimatedArrival)
	} else if f.ScheduledArrival != nil {
		view.ArrivalLabel, view.Arrival = "Scheduled arrival", format(f.ScheduledArrival)
	}
	if f.ScheduledArrival != nil && view.ArrivalLabel != "Scheduled arrival" && view.Arrival != format(f.ScheduledArrival) {
		view.Scheduled = format(f.ScheduledArrival)
	}
	return view
}
