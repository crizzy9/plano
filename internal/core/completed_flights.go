package core

import (
	"context"

	"github.com/crizzy9/plano/internal/feeds"
)

// CompletedFlights returns the installation's full Passport history,
// independently of the tracked-flight dashboard's row limit.
func (s *Store) CompletedFlights(ctx context.Context, owner string) ([]feeds.Flight, error) {
	return s.list(ctx, `SELECT f.snapshot FROM tracked t JOIN flights f ON t.flight_id=f.id
WHERE t.owner=? AND json_extract(f.snapshot,'$.status')='arrived'
ORDER BY COALESCE(julianday(json_extract(f.snapshot,'$.actualArrival')), julianday(json_extract(f.snapshot,'$.scheduledArrival'))) DESC,
         f.observed_ns DESC,f.id`, owner)
}
