package feeds

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const AdvisoryAirportNotice = "airport_notice"

var (
	ErrAdvisoryNotYetEffective = errors.New("advisory replacement or cancellation is not effective yet")
	aixmPublisher              = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$\z`)
	aixmSerialNumber           = regexp.MustCompile(`^[0-9]{1,4}$\z`)
	aixmSerialYear             = regexp.MustCompile(`^(?:20)?[0-9]{2}$\z`)
	aixmDaily                  = regexp.MustCompile(`(?i)^(?:DAILY|DLY) ([0-9]{2}):?([0-9]{2})\s*[-–]\s*([0-9]{2}):?([0-9]{2})$\z`)
)

// AIXMAdvisoryBodies converts recent aerodrome notices to the signed webhook's
// JSON contract. The publisher label belongs to the operator's upstream feed.
func AIXMAdvisoryBodies(input io.Reader, source, publisher string, fallbackIssued, now time.Time) ([][]byte, error) {
	if source != FAASWIM && source != Eurocontrol || !aixmPublisher.MatchString(publisher) {
		return nil, ErrInvalidQuery
	}
	notices, err := readAIXMNOTAMs(input)
	if err != nil {
		return nil, err
	}
	type entry struct {
		advisory Advisory
		body     []byte
	}
	type revision struct {
		id     string
		issued time.Time
	}
	var entries []entry
	var publisherRef string
	var deferred bool
	seen := make(map[revision][]byte)
	for _, notice := range notices {
		if notice.Publisher != "" {
			if publisherRef != "" && publisherRef != notice.Publisher {
				return nil, ErrInvalidQuery
			}
			publisherRef = notice.Publisher
		}
		if notice.Scope != "A" {
			continue
		}
		issued := fallbackIssued.UTC()
		if notice.Issued != "" {
			issued, err = time.Parse(time.RFC3339Nano, notice.Issued)
			if err != nil {
				return nil, ErrInvalidQuery
			}
			issued = issued.UTC()
		} else if len(notices) != 1 {
			// A batch can include earlier slices. One transport timestamp
			// cannot establish the individual notices' publication times.
			return nil, ErrInvalidQuery
		}
		if issued.IsZero() || issued.After(now.Add(30*time.Second)) {
			return nil, ErrInvalidQuery
		}
		if issued.Before(now.Add(-6 * time.Hour)) {
			continue
		}
		advisory, err := notice.advisory(source, publisher, issued)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(advisoryIngressEnvelope{Version: 1, Source: source, Advisory: &advisory})
		if err != nil {
			return nil, err
		}
		key := revision{advisory.ID, issued}
		if previous, exists := seen[key]; exists {
			if !bytes.Equal(body, previous) {
				return nil, ErrInvalidQuery
			}
			continue
		}
		seen[key] = body
		if advisory.SupersedesID != "" {
			if start := notamTimestamp(notice.EffectiveStart); start != nil && start.After(now) {
				deferred = true
				continue
			}
		}
		entries = append(entries, entry{advisory, body})
	}
	if len(entries) == 0 && !deferred {
		return nil, ErrInvalidQuery
	}
	// Publish newer notices and their supersession links first, so replaying
	// a source history does not enqueue an already-superseded alert.
	slices.SortFunc(entries, func(a, b entry) int {
		if order := b.advisory.ObservedAt.Compare(a.advisory.ObservedAt); order != 0 {
			return order
		}
		if a.advisory.SupersedesID != b.advisory.SupersedesID {
			if a.advisory.SupersedesID == "" {
				return 1
			}
			if b.advisory.SupersedesID == "" {
				return -1
			}
		}
		return strings.Compare(b.advisory.ID, a.advisory.ID)
	})
	bodies := make([][]byte, len(entries))
	for i, entry := range entries {
		bodies[i] = entry.body
	}
	if deferred {
		return bodies, ErrAdvisoryNotYetEffective
	}
	return bodies, nil
}

func (n aixmNOTAM) advisory(source, publisher string, issued time.Time) (Advisory, error) {
	airport := strings.ToUpper(n.Location)
	serial, ok := aixmSerial(n.Series, n.Number, n.Year)
	if !ok || !notamICAOLocation.MatchString(airport) {
		return Advisory{}, ErrInvalidQuery
	}
	row := Advisory{ID: publisher + "/" + serial, Airport: airport, Source: source,
		ObservedAt: issued, Kind: AdvisoryAirportNotice}
	header := serial + " NOTAM" + n.Type
	switch n.Type {
	case "N":
		if n.ReferredSeries != "" || n.ReferredNumber != "" || n.ReferredYear != "" {
			return Advisory{}, ErrInvalidQuery
		}
	case "R", "C":
		ref, ok := aixmSerial(n.ReferredSeries, n.ReferredNumber, n.ReferredYear)
		if !ok || ref == serial || ref[0] != serial[0] {
			return Advisory{}, ErrInvalidQuery
		}
		row.SupersedesID = publisher + "/" + ref
		header += " " + ref
	default:
		return Advisory{}, ErrInvalidQuery
	}

	start := notamTimestamp(n.EffectiveStart)
	if start == nil && (n.Type != "C" || n.EffectiveStart != "") {
		return Advisory{}, ErrInvalidQuery
	}
	end, ok := n.endField()
	if !ok || n.Type == "C" && end != "" {
		return Advisory{}, ErrInvalidQuery
	}
	if endTime := notamTimestamp(strings.TrimSpace(strings.TrimSuffix(end, "EST"))); start != nil && endTime != nil && !endTime.After(*start) {
		return Advisory{}, ErrInvalidQuery
	}

	var raw strings.Builder
	raw.WriteString(header + "\nA) " + airport)
	schedule := n.Schedule
	if daily := aixmDaily.FindStringSubmatch(schedule); daily != nil {
		startTime, endTime := daily[1]+daily[2], daily[3]+daily[4]
		if endTime == "2359" {
			endTime = "0000"
		}
		if startTime == "0000" && daily[3]+daily[4] == "2359" {
			schedule = "" // A full UTC day is continuous over the published validity.
		} else {
			schedule = "DLY " + startTime + "-" + endTime
		}
	}
	for _, field := range []struct{ key, value string }{
		{"B", n.EffectiveStart}, {"C", end}, {"D", schedule}, {"E", n.Text}, {"F", n.LowerLimit}, {"G", n.UpperLimit},
	} {
		if field.value == "" {
			continue
		}
		if notamField.MatchString(field.value) {
			return Advisory{}, ErrInvalidQuery
		}
		raw.WriteString("\n" + field.key + ") " + field.value)
	}
	row.RawReason = raw.String()
	if n.Type == "C" {
		row.Kind = "notam_cancellation"
		row.Reason = notamCancellationText
	} else {
		if n.Text == "" {
			return Advisory{}, ErrInvalidQuery
		}
		row.StartsAt = start
		if summary, ok := DecodeNOTAM(row.RawReason); ok {
			row.Reason = summary.Text
			if summary.FixedValidity {
				row.EndsAt = summary.EndsAt
			}
			defaultLower := n.MinimumFL == "" || n.MinimumFL == "000" || n.MinimumFL == "0"
			defaultUpper := n.MaximumFL == "" || n.MaximumFL == "999"
			unqualified := (n.Traffic == "" || n.Traffic == "IV") && (n.SelectionCode == "" || n.SelectionCode == "QFALC")
			if summary.FullAirportClosure && summary.FixedValidity && unqualified && defaultLower && defaultUpper &&
				n.LowerLimit == "" && n.UpperLimit == "" {
				row.Kind = "airport_closure"
			}
		} else {
			row.Reason = advisoryPlainText(n.Text)
			if schedule != "" {
				row.Reason = strings.TrimRight(row.Reason, ".") + ". Published schedule " + advisoryPlainText(schedule) + "."
			}
		}
	}
	if !normalizeBridgeAdvisory(&row, source) {
		return Advisory{}, ErrInvalidQuery
	}
	return row, nil
}

func (n aixmNOTAM) endField() (string, bool) {
	for _, flag := range []string{n.EstimatedEnd, n.Permanent} {
		if flag != "" && flag != "YES" && flag != "NO" {
			return "", false
		}
	}
	end := n.EffectiveEnd
	if n.Permanent == "YES" || end == "PERM" {
		return "PERM", (end == "" || end == "PERM") && n.EstimatedEnd != "YES" && n.Permanent != "NO"
	}
	end, hasSuffix := strings.CutSuffix(end, "EST")
	end = strings.TrimSpace(end)
	if end == "" && !hasSuffix && n.EstimatedEnd != "YES" {
		return "", true
	}
	parsed := notamTimestamp(end)
	if parsed == nil || hasSuffix && n.EstimatedEnd == "NO" {
		return "", false
	}
	if hasSuffix || n.EstimatedEnd == "YES" {
		end += " EST"
	} else if strings.HasSuffix(end, "2359") {
		// ICAO Item C represents midnight as 23:59 on the previous day.
		midnight := parsed.Add(time.Minute)
		if midnight.Year() > 2099 {
			return "", false
		}
		end = midnight.Format("0601021504")
	}
	return end, true
}

func aixmSerial(series, number, year string) (string, bool) {
	if len(series) != 1 || series[0] < 'A' || series[0] > 'Z' || !aixmSerialNumber.MatchString(number) ||
		!aixmSerialYear.MatchString(year) {
		return "", false
	}
	n, err := strconv.Atoi(number)
	if err != nil || n == 0 {
		return "", false
	}
	return fmt.Sprintf("%s%04d/%s", series, n, year[len(year)-2:]), true
}
