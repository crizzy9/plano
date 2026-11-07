package feeds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	faaNASEndpoint = "https://nasstatus.faa.gov/api/airport-status-information"
	faaNASSource   = "faa_nas"
)

// FAANAS consumes the FAA's public airport status XML snapshot.
type FAANAS struct {
	endpoint string
	client   *http.Client
}

func NewFAANAS(client *http.Client) *FAANAS {
	return &FAANAS{endpoint: faaNASEndpoint, client: feedClient(client)}
}

func (f *FAANAS) AdvisorySourceID() string { return faaNASSource }

func (f *FAANAS) Advisories(ctx context.Context) ([]Advisory, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/xml")
	body, err := responseBody(f.client, request, faaNASSource)
	if err != nil {
		return nil, err
	}
	return parseFAANAS(body)
}

type faaAirportEvent struct {
	Airport string `xml:"ARPT"`
	Reason  string `xml:"Reason"`
	Start   string `xml:"Start"`
	End     string `xml:"End_Time"`
	Reopen  string `xml:"Reopen"`
	Average string `xml:"Avg"`
	Delays  []struct {
		Type string `xml:"Type,attr"`
		Min  string `xml:"Min"`
	} `xml:"Arrival_Departure"`
}

type faaAreaEvent struct {
	Area      string `xml:"CTL_Element"`
	Program   string `xml:"Program_Name"`
	Reason    string `xml:"Reason"`
	Start     string `xml:"AFP_StartTime"`
	End       string `xml:"AFP_EndTime"`
	CTOPStart string `xml:"CTOP_Start_Time"`
	CTOPEnd   string `xml:"CTOP_End_Time"`
	Average   string `xml:"Avg"`
}

func parseFAANAS(body []byte) ([]Advisory, error) {
	var status struct {
		XMLName xml.Name `xml:"AIRPORT_STATUS_INFORMATION"`
		Updated string   `xml:"Update_Time"`
		Groups  []struct {
			Stops       []faaAirportEvent `xml:"Ground_Stop_List>Program"`
			GroundDelay []faaAirportEvent `xml:"Ground_Delay_List>Ground_Delay"`
			Closures    []faaAirportEvent `xml:"Airport_Closure_List>Airport"`
			Delays      []faaAirportEvent `xml:"Arrival_Departure_Delay_List>Delay"`
			Airspace    []faaAreaEvent    `xml:"Airspace_Flow_List>Airspace_Flow"`
			CTOP        []faaAreaEvent    `xml:"CTOP_List>CTOP"`
		} `xml:"Delay_type"`
	}
	if err := xml.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("%w: %s returned invalid airport status XML", ErrUnavailable, faaNASSource)
	}
	observed, err := time.Parse("Mon Jan 2 15:04:05 2006 MST", strings.Join(strings.Fields(status.Updated), " "))
	if err != nil {
		return nil, fmt.Errorf("%w: %s response has an invalid update time", ErrUnavailable, faaNASSource)
	}
	observed = observed.UTC()
	advisories := make([]Advisory, 0)
	seen := make(map[string]int)
	add := func(a Advisory, stablePart string) {
		if a.Airport == "" && a.Area == "" {
			return
		}
		id := sha256.Sum256([]byte(strings.Join([]string{a.Airport, a.Area, a.Kind, a.Direction, a.RawReason, stablePart}, "\x00")))
		a.ID = hex.EncodeToString(id[:16])
		if index, exists := seen[a.ID]; exists {
			prior := &advisories[index]
			if a.EndsAt != nil && (prior.EndsAt == nil || a.EndsAt.After(*prior.EndsAt)) {
				prior.EndsAt = a.EndsAt
			}
			if a.DelayMinutes != nil && (prior.DelayMinutes == nil || *a.DelayMinutes > *prior.DelayMinutes) {
				prior.DelayMinutes = a.DelayMinutes
			}
			return
		}
		seen[a.ID] = len(advisories)
		a.Source, a.ObservedAt = faaNASSource, observed
		advisories = append(advisories, a)
	}
	for _, group := range status.Groups {
		for _, event := range group.Stops {
			row := event.advisory("ground_stop", "arrival")
			row.EndsAt = faaTime(event.End, observed)
			add(row, "")
		}
		for _, event := range group.GroundDelay {
			row := event.advisory("ground_delay", "arrival")
			row.DelayMinutes = faaMinutes(event.Average)
			add(row, "")
		}
		for _, event := range group.Closures {
			row := event.advisory("airport_closure", "")
			row.StartsAt, row.EndsAt = faaTime(event.Start, observed), faaTime(event.Reopen, observed)
			summary, decoded := DecodeNOTAM(row.RawReason)
			if summary.HasDailyClosure() {
				// Retain a disagreement so flight applicability can reject it.
				if summary.MatchesValidity(row.StartsAt, row.EndsAt) {
					row.StartsAt, row.EndsAt = summary.StartsAt, summary.EndsAt
				}
				row.Reason = summary.Text
			} else if strings.HasPrefix(row.RawReason, "!") {
				row.Kind = "airport_restriction"
				if start, end := notamTimes(row.RawReason); start != nil {
					row.StartsAt, row.EndsAt = start, end
				}
				row.Reason = explainClosure(row.RawReason)
				if decoded {
					row.Reason = summary.Text
					if summary.FullAirportClosure {
						row.Kind = "airport_closure"
					}
				}
			} else if decoded && !summary.FullAirportClosure {
				row.Kind, row.Reason = "airport_restriction", summary.Text
			}
			add(row, strings.TrimSpace(event.Start))
		}
		for _, event := range group.Delays {
			for _, delay := range event.Delays {
				direction := strings.ToLower(strings.TrimSpace(delay.Type))
				if direction != "arrival" && direction != "departure" {
					continue
				}
				row := event.advisory(direction+"_delay", direction)
				row.DelayMinutes = faaMinutes(delay.Min)
				add(row, "")
			}
		}
		for _, events := range []struct {
			rows []faaAreaEvent
			kind string
		}{{group.Airspace, "airspace_flow"}, {group.CTOP, "ctop"}} {
			for _, event := range events.rows {
				reason := strings.TrimSpace(event.Reason)
				start, end := first(event.Start, event.CTOPStart), first(event.End, event.CTOPEnd)
				row := Advisory{Area: first(event.Area, event.Program), Kind: events.kind, Reason: reason, RawReason: reason,
					StartsAt: faaTime(start, observed), EndsAt: faaTime(end, observed), DelayMinutes: faaMinutes(event.Average)}
				if events.kind == "ctop" {
					// Published HHMM parent clocks do not establish a calendar day.
					if faaCTOPClock.MatchString(start) {
						row.RawStartTime = start
					}
					if faaCTOPClock.MatchString(end) {
						row.RawEndTime = end
					}
				}
				add(row, start)
			}
		}
	}
	return advisories, nil
}

func (e faaAirportEvent) advisory(kind, direction string) Advisory {
	code := strings.ToUpper(strings.TrimSpace(e.Airport))
	if !airportCode.MatchString(code) {
		code = ""
	}
	reason := strings.TrimSpace(e.Reason)
	return Advisory{Airport: code, Kind: kind, Reason: reason, RawReason: reason, Direction: direction}
}

var (
	faaDuration  = regexp.MustCompile(`(?i)^(?:(\d{1,2})\s*(?:hours?|hrs?|h)(?:\s+and\s+|\s*))?(?:(\d{1,4})\s*(?:minutes?|mins?|m))?$\z`)
	notamRange   = regexp.MustCompile(`\b([0-9]{10}) ?[-‐‑–] ?([0-9]{10})(?: ?EST)?\b`)
	faaZulu      = regexp.MustCompile(`^([0-9]{2})([0-9]{2})([0-9]{2})Z$\z`)
	faaCTOPClock = regexp.MustCompile(`^(?:[01][0-9]|2[0-3])[0-5][0-9]$\z`)
)

func faaMinutes(raw string) *int {
	match := faaDuration.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil || match[1] == "" && match[2] == "" {
		return nil
	}
	hours, _ := strconv.Atoi(match[1])
	minutes, _ := strconv.Atoi(match[2])
	total := hours*60 + minutes
	if total > 1440 {
		return nil
	}
	return &total
}

func faaTime(raw string, observed time.Time) *time.Time {
	value := strings.TrimSuffix(strings.Join(strings.Fields(raw), " "), ".")
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "Jan 02 2006 at 15:04 UTC", "200601021504", "0601021504"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return &parsed
		}
	}
	if parsed, err := time.Parse("Jan 2 at 15:04 UTC", value); err == nil {
		return nearestCalendarTime(observed, parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute())
	}
	if parts := faaZulu.FindStringSubmatch(value); parts != nil {
		day, _ := strconv.Atoi(parts[1])
		hour, _ := strconv.Atoi(parts[2])
		minute, _ := strconv.Atoi(parts[3])
		if day < 1 || day > 31 || hour > 23 || minute > 59 {
			return nil
		}
		var nearest time.Time
		for _, offset := range []int{-1, 0, 1} {
			month := time.Date(observed.Year(), observed.Month()+time.Month(offset), 1, 0, 0, 0, 0, time.UTC)
			candidate := time.Date(month.Year(), month.Month(), day, hour, minute, 0, 0, time.UTC)
			if candidate.Month() == month.Month() && (nearest.IsZero() || candidate.Sub(observed).Abs() < nearest.Sub(observed).Abs()) {
				nearest = candidate
			}
		}
		if !nearest.IsZero() {
			return &nearest
		}
	}
	return nil
}

func nearestCalendarTime(observed time.Time, month time.Month, day, hour, minute int) *time.Time {
	var nearest time.Time
	for _, year := range []int{observed.Year() - 1, observed.Year(), observed.Year() + 1} {
		candidate := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
		if candidate.Month() == month && (nearest.IsZero() || candidate.Sub(observed).Abs() < nearest.Sub(observed).Abs()) {
			nearest = candidate
		}
	}
	if nearest.IsZero() {
		return nil
	}
	return &nearest
}

func notamTimes(raw string) (*time.Time, *time.Time) {
	parts := notamRange.FindStringSubmatch(raw)
	if parts == nil {
		return nil, nil
	}
	start, end := notamTimestamp(parts[1]), notamTimestamp(parts[2])
	if start == nil || end == nil || !end.After(*start) {
		return nil, nil
	}
	return start, end
}

func explainClosure(raw string) string {
	// Report only recognizable closure patterns. Preserve exact exceptions and
	// applicability in RawReason instead of turning them into whole-airport bans.
	switch {
	case strings.Contains(raw, "AD AP CLSD TO NON SKED TRANSIENT GA ACFT"):
		return "Airport access is restricted for non-scheduled visiting general aviation aircraft."
	case strings.Contains(raw, "AD AP CLSD DLY"):
		return "The airport is closed during specified daily hours."
	case strings.Contains(raw, "AD AP CLSD TO"):
		return "The airport closure applies to specified operations."
	case strings.Contains(raw, "AD AP CLSD"):
		return "The FAA issued an airport closure notice."
	default:
		return raw
	}
}
