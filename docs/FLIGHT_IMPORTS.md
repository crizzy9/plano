# Import flights into Plano

Open **Tools → Import flights**. Plano imports only uniquely matched flight instances, up to eight distinct dated legs per request. Split larger pasted itineraries into separate requests. A pasted confirmation can contain separate dates for each leg. For iCalendar files, a UTC start or recognized IANA time zone lets Plano match the scheduled departure instant and check adjacent origin dates when needed. All-day events, floating times, and unrecognized time zones use the event date. The parser skips cancelled and recurring calendar entries. When one calendar contains multiple active revisions of an event, Plano prefers the highest sequence number, then the latest valid UTC timestamp for matching sequences. [iCalendar specification](https://www.rfc-editor.org/rfc/rfc5545.html) · [Calendar revision ordering](https://www.rfc-editor.org/rfc/rfc5546.html#section-2.1.5)

The mobile calendar integration requests permission before reading event titles and departure times. On-demand and optional automatic scans skip events the device marks canceled and send only recognized flight designators with departure dates or times to the selected self-host. Automatic sync checks the next 30 days in small batches when you open the app and about every 30 minutes while it stays open. iOS and Android decide when the background task can run. Background sync enabled in an earlier release keeps sending dates until you turn it off and back on.

The server records a temporary receipt for each successfully matched automatic import. Retrying the same flight number, date, and optional departure time does not add that entry back after you untrack it. Unresolved entries can be retried. Receipts are scoped to your installation, expire after the entry's date passes, and contain a digest and expiry time. Update the app and server together for these guarantees. Older servers continue to support the app's legacy calendar import path.

For an overnight flight, label the departure and arrival separately. Plano uses the departure date. If the confirmation supplies a local departure time, Plano matches the scheduled minute in the flight’s origin time zone. A missing origin time zone or multiple matching flights leaves that leg unresolved. Forwarded HTML tables with Flight, Departure, and Arrival headers can supply the same information.

```text
UA456 | Departure: 7 January 2027 5:00 PM | Arrival: 8 January 2027 5:00 AM
```

A labeled departure can also include a timestamp with a numeric UTC offset, such as `2027-01-07T17:00:00-05:00`. [RFC 3339 timestamp format](https://www.rfc-editor.org/rfc/rfc3339.html#section-5.6)

## Log a past flight manually

Open **Trip tools → Plano Passport → Log past flight**, or choose **Log past
flight in Passport** after a past-flight search returns no match. Enter the
departure date and the departure and arrival airport codes. The date must be
before the current UTC day. Flight number, aircraft registration and model,
gate-to-gate duration, and minutes late at arrival are optional. Enter zero
for an on-time or early arrival, and leave unknown measurements blank.

Saved flights appear in the installation’s private Passport. Known airports
use the server’s cached or bundled coordinates, and routes with missing
coordinates still count toward the history. Edit or remove a manual flight
from its history row. Manual entries and CSV imports share the 10,000-flight
archive limit. [Flighty manual-entry behavior](https://flighty.com/help/using-search)

## Import Passport history from Flighty

Save your Flighty export as a CSV file. Open **Plano Passport** on iOS or
Android, choose **Import Flighty CSV**, and select the file. Plano reports how
many flights it added, recognized as duplicates, or skipped. Files can be up
to 512 KiB and 1,000 rows. Each installation can store up to 10,000 imported
flights. [Public Flighty export format example](https://github.com/SSARCandy/flighty-web-viewer)

The file goes to your selected Plano server. The importer retains the flight
date, route, recognized designator, aircraft details, and usable times. It
discards the CSV and ignores booking codes, seats, and notes. It skips current
or future departures, cancellations, diversions, and unrecognized rows. Imported
flights appear only in your installation's Passport. Remove an imported record
from its history row. The import creates no tracked flight or notification.

Plano uses the departure and arrival airports' time zones to resolve local
gate times in Flighty exports. When the times can be resolved, Passport counts
gate-to-gate hours and arrival delays. Missing times, clocks that repeat or
are skipped during an offset change, and offset-free scheduled midnight
placeholders are omitted from the time calculations. If you imported a file
before Plano could resolve local times, import it again to fill in times for
the matching records with no saved timestamps. Those flights still count as
already in Passport. [Flighty CSV example](https://github.com/SSARCandy/flighty-web-viewer) · [Observed midnight placeholder](https://github.com/vlvovch/flight-ledger/blob/main/docs/importers.md#flighty)

When a completed tracked flight matches the route, flight number, and
scheduled-departure instant, Passport shows the tracked record. The imported
copy becomes visible and removable if you untrack that flight. If a scheduled
departure instant cannot be resolved, both entries can appear and you can
remove the imported copy in Passport.

Plano maps imported routes using cached or bundled airport coordinates. The
bundled catalog includes 8,154 unambiguous IATA and ICAO codes for airports
with scheduled service. Unknown airports remain in history with missing map
coverage. The catalog was derived from the public-domain OurAirports CSV
downloaded on November 6, 2026, with SHA-256
`e2e65a3ddaf76170c0e2c373cb8b68597bdf3b59d243c4d6db454d48498ba6d7`.
[OurAirports data](https://ourairports.com/data/)

The offline time-zone table matches 7,667 of those codes to `airportsdata`
entries with the same code, coordinates within 8 km, and one unambiguous IANA
zone. The source CSV at revision `dcc46ce603ca445c0d9d9e074932452ab7d8c2d8`
has SHA-256 `b218baf3b864af711bfc5c2d6de38f4cdfde873e0c60289fd9c5cc22afe57876`.
The table uses current airport assignments with historical offset rules for
the chosen zones. [Source catalog](https://github.com/mborsetti/airportsdata/tree/dcc46ce603ca445c0d9d9e074932452ab7d8c2d8) · [Attribution](../internal/itinerary/passport_airport_zones.LICENSE)

## Forward confirmation emails

In the import screen, generate an email relay token. Each installation has one relay credential. Generating a replacement invalidates the previous token. A relay token can submit email for import. Installation credentials are required to view tracked flights or bookings.

Configure your inbound mail service or local MTA to send the complete RFC 5322 message as `message/rfc822` over HTTPS. With Postfix, pass the incoming message on stdin from a pipe transport for a private import recipient. Restrict delivery to trusted forwarding sources because every accepted message can add a tracked flight. Run the pipe under a dedicated relay account and put only the relay token in a private file readable by that account. A wrapper can invoke the Plano binary as follows. [Postfix pipe delivery](https://www.postfix.net/pipe.8.html)

```sh
export PLANO_EMAIL_RELAY_URL=https://plano.example.net/v1/webhooks/email
export PLANO_EMAIL_RELAY_TOKEN_FILE=/run/secrets/plano-relay-token
exec /usr/local/bin/plano forward-email
```

Send at most 256 KiB per message. `forward-email` exits with `75` for temporary failures so the mail system can retry. It reports `65` for rejected input, `77` for an unauthorized relay, and `78` for configuration or unexpected endpoint errors. Diagnostics omit email contents and credentials.

The parser recognizes plain text, HTML text, forwarded messages, and calendar parts. It never follows links in the message. A `204` acknowledges an accepted delivery. A recognized cancellation with no importable legs also returns `204` and leaves existing tracking in place. A partial match keeps the resolved flights and returns `503`. A retry of the same bytes resolves only unfinished legs, even if you have since untracked a resolved flight. For `422`, review the original message and enter the flight manually if needed. Plano keeps digests for deduplication and discards the email body. [iCalendar cancellation method](https://www.rfc-editor.org/rfc/rfc5546.html#section-3.2.5)

Plain-text notices such as “Flight UA123 on 5 January 2027 has been
cancelled” can suppress that dated leg. With a departure time in the notice,
Plano can keep a different same-day departure. When the notice supplies only
a date, copies with the same designator and date remain ambiguous.
Clearly dated replacement flights can still be imported from the same email.

## Import a TripIt calendar feed

Enable TripIt's calendar feed and detailed itinerary items, then copy its private calendar URL into the TripIt field in Plano. Keep the URL private. A one-time import reads the feed over HTTPS and adds recognized flights in pages of eight, starting with upcoming trips. Choose **Import next TripIt flights** to continue through your history. A one-time import uses the URL only for the request. [TripIt calendar feed setup](https://help.tripit.com/en/support/solutions/articles/103000063280-calendar-feed-setup-and-sync) · [TripIt detailed items](https://help.tripit.com/en/support/solutions/articles/103000063363-plans-not-appearing-in-calendar)

If your server enables **Automatic TripIt sync**, you can save the same feed for recurring imports. The server encrypts the URL and checks for new upcoming flights. When a flight search times out, the next check can continue with later entries. The sync status reports the last check and the next scheduled check. Choose **Stop TripIt sync** to remove the stored feed and stop future imports. Flights already imported remain in your list. [Self-host configuration](SELF_HOSTING.md#airport-routing-imports-and-delay-models)

## Review possible connections

Open **Tools → Connection Assistant** after an import. Plano suggests tracked flights that connect at the same airport and have an onward departure in the next seven days. A shared imported confirmation code can match layovers up to 24 hours. Matches based on the schedule alone have a limit of eight hours. Review the flights and choose **Monitor connection** if you want risk alerts. [Connection monitoring](AIRPORT_TRANSFERS.md#monitor-a-connection)

## Find an imported booking by confirmation code

Set `PLANO_BOOKING_KEY_FILE` to a mounted private file containing a stable random secret of at least 32 bytes. Plano stores an installation-scoped keyed digest of a recognized six-character locator. The booking lookup returns flights already imported and still tracked by that installation. Rotating the key makes previous locator digests unavailable for lookup until a fresh import.

## Look up a reservation through a partner

Open **Partner booking lookup** and enter the carrier code, six-character confirmation code, and traveler last name. With a configured bridge, the server sends those details to the operator's licensed reservation partner. Route and departure-time details from the partner help Plano distinguish flights that share a number and date. Review matched flights and select the ones you want to track. Plano does not save the submitted reservation credentials. [Reservation gateway contract](RESERVATION_GATEWAY.md)
