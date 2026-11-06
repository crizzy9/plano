package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/api"
	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/demodata"
	"github.com/crizzy9/plano/internal/feeds"
)

func TestCalendarSyncRetriesOnlyUnmatchedKeysAndKeepsReceiptsAfterUntrack(t *testing.T) {
	source := &evolvingImportSource{}
	store, handler := calendarSyncFixture(t, source)
	owner, token := calendarSyncInstallation(t, store)
	other, otherToken := calendarSyncInstallation(t, store)
	day := time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly)
	firstKey, secondKey := day+" UA123", day+" UA456 220000"
	body := fmt.Sprintf(`{"flights":[{"day":%q,"code":"UA123"},{"day":%q,"code":"UA456","utcTime":"220000"},{"day":%q,"code":"UA123"}]}`, day, day, day)
	checkCalendarSync(t, handler, token, body, 200, firstKey)
	if err := store.Untrack(t.Context(), owner, "demo-UA123-"+day); err != nil {
		t.Fatal(err)
	}
	checkCalendarSync(t, handler, token, body, 200, firstKey)
	source.secondLeg = true
	checkCalendarSync(t, handler, token, body, 200, firstKey, secondKey)
	checkCalendarSync(t, handler, token, body, 200, firstKey, secondKey)
	assertCalendarTracks(t, store, owner, "demo-UA456-"+day)
	if source.firstLegLookups != 1 {
		t.Fatalf("first leg looked up %d times, want 1", source.firstLegLookups)
	}
	checkCalendarSync(t, handler, otherToken, body, 200, firstKey, secondKey)
	assertCalendarTracks(t, store, other, "demo-UA123-"+day, "demo-UA456-"+day)
	if source.firstLegLookups != 2 {
		t.Fatalf("first leg looked up %d times after second installation, want 2", source.firstLegLookups)
	}
}

func TestCalendarSyncValidatesBatchBeforeLookup(t *testing.T) {
	source := &evolvingImportSource{}
	store, handler := calendarSyncFixture(t, source)
	_, token := calendarSyncInstallation(t, store)
	today := time.Now().UTC()
	day := today.AddDate(0, 0, 1).Format(time.DateOnly)
	valid := fmt.Sprintf(`{"day":%q,"code":"UA123"}`, day)
	checkCalendarSync(t, handler, "", `{"flights":[`+valid+`]}`, 401)
	for _, invalid := range []string{
		fmt.Sprintf(`{"day":%q,"code":"ua123"}`, day),
		fmt.Sprintf(`{"day":%q,"code":"UA123","utcTime":"246000"}`, day),
		fmt.Sprintf(`{"day":%q,"code":"UA123","utcTime":"15000"}`, day),
		fmt.Sprintf(`{"day":%q,"code":"UA123","title":"private calendar event"}`, day),
		fmt.Sprintf(`{"day":%q,"code":"UA123"}`, today.AddDate(0, 0, -2).Format(time.DateOnly)),
		fmt.Sprintf(`{"day":%q,"code":"UA123"}`, today.AddDate(0, 0, 34).Format(time.DateOnly)),
	} {
		checkCalendarSync(t, handler, token, `{"flights":[`+valid+`,`+invalid+`]}`, 400)
	}
	checkCalendarSync(t, handler, token, `{"flights":[`+strings.Repeat(valid+",", 8)+valid+`]}`, 400)
	checkCalendarSync(t, handler, token, `{"flights":[]}`, 400)
	if source.firstLegLookups != 0 {
		t.Fatalf("invalid requests looked up %d flights", source.firstLegLookups)
	}
}

type calendarAliasSource struct{ demodata.Source }

func (source calendarAliasSource) Search(ctx context.Context, number string, day time.Time) ([]feeds.Flight, error) {
	switch number {
	case "DL100":
		number = "UA123"
	case "DL200":
		return nil, feeds.ErrNotFound
	case "DL201":
		return nil, feeds.ErrUnavailable
	case "DL300":
		first, err := source.Source.Search(ctx, "UA123", day)
		if err != nil {
			return nil, err
		}
		second, err := source.Source.Search(ctx, "UA456", day)
		return append(first, second...), err
	}
	return source.Source.Search(ctx, number, day)
}

func TestCalendarSyncAcknowledgesDistinctKeysForSameFlight(t *testing.T) {
	store, handler := calendarSyncFixture(t, calendarAliasSource{})
	owner, token := calendarSyncInstallation(t, store)
	day := time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly)
	body := fmt.Sprintf(`{"flights":[{"day":%q,"code":"DL100"},{"day":%q,"code":"UA123","utcTime":"150000"},{"day":%q,"code":"DL200"},{"day":%q,"code":"DL300"}]}`, day, day, day, day)
	checkCalendarSync(t, handler, token, body, 200, day+" DL100", day+" UA123 150000")
	assertCalendarTracks(t, store, owner, "demo-UA123-"+day)
	unresolved := fmt.Sprintf(`{"flights":[{"day":%q,"code":"DL200"}]}`, day)
	checkCalendarSync(t, handler, token, unresolved, 200)
}

func TestCalendarSyncCommitsAvailableLegsDuringPartialProviderFailure(t *testing.T) {
	store, handler := calendarSyncFixture(t, calendarAliasSource{})
	owner, token := calendarSyncInstallation(t, store)
	day := time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly)
	unavailable := fmt.Sprintf(`{"day":%q,"code":"DL201"}`, day)
	available := fmt.Sprintf(`{"day":%q,"code":"UA123"}`, day)
	checkCalendarSync(t, handler, token, `{"flights":[`+unavailable+`]}`, 503)
	checkCalendarSync(t, handler, token, `{"flights":[`+unavailable+`,`+available+`]}`, 200, day+" UA123")
	assertCalendarTracks(t, store, owner, "demo-UA123-"+day)
}

type blockedCalendarSource struct {
	demodata.Source
	lookups atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (source *blockedCalendarSource) Search(ctx context.Context, number string, day time.Time) ([]feeds.Flight, error) {
	if source.lookups.Add(1) == 1 {
		close(source.entered)
		select {
		case <-source.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return source.Source.Search(ctx, number, day)
}

func TestCalendarSyncLateDuplicateDoesNotRetrack(t *testing.T) {
	source := &blockedCalendarSource{entered: make(chan struct{}), release: make(chan struct{}, 1)}
	defer close(source.release)
	store, handler := calendarSyncFixture(t, source)
	owner, token := calendarSyncInstallation(t, store)
	day := time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly)
	body := fmt.Sprintf(`{"flights":[{"day":%q,"code":"UA123"}]}`, day)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/import/calendar-sync", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	late := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler.ServeHTTP(late, request)
	}()
	select {
	case <-source.entered:
	case <-finished:
		t.Fatalf("late request finished before its lookup: %d %s", late.Code, late.Body)
	}
	checkCalendarSync(t, handler, token, body, 200, day+" UA123")
	if err := store.Untrack(t.Context(), owner, "demo-UA123-"+day); err != nil {
		t.Fatal(err)
	}
	source.release <- struct{}{}
	<-finished
	if late.Code != http.StatusOK {
		t.Fatalf("late calendar sync = %d (%s)", late.Code, late.Body)
	}
	assertCalendarTracks(t, store, owner)
}

func calendarSyncFixture(t *testing.T, source feeds.FlightSource) (*core.Store, http.Handler) {
	t.Helper()
	store, err := core.Open(filepath.Join(t.TempDir(), "plano.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, (api.Server{Store: store, Source: source, Demo: true, Logger: slog.New(slog.DiscardHandler)}).Handler()
}

func calendarSyncInstallation(t *testing.T, store *core.Store) (owner, token string) {
	t.Helper()
	token, err := store.CreateInstallation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err = store.Installation(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	return owner, token
}

func checkCalendarSync(t *testing.T, handler http.Handler, token, body string, wantStatus int, wantKeys ...string) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/import/calendar-sync", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != wantStatus {
		t.Fatalf("calendar sync = %d (%s), want %d", w.Code, w.Body, wantStatus)
	}
	if w.Code == http.StatusOK {
		var reply struct{ Completed []string }
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || reply.Completed == nil || !slices.Equal(reply.Completed, wantKeys) {
			t.Fatalf("calendar sync = %q (%v), want completed %q", w.Body, err, wantKeys)
		}
	}
}

func assertCalendarTracks(t *testing.T, store *core.Store, owner string, want ...string) {
	t.Helper()
	flights, err := store.Tracked(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(flights))
	for _, flight := range flights {
		ids = append(ids, flight.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, want) {
		t.Fatalf("tracked %q, want %q", ids, want)
	}
}
