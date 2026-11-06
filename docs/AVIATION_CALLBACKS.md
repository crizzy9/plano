# Event-driven aviation updates

Plano accepts two optional inbound callback types in live mode. Both use the same SQLite event and push pipeline as periodic ingestion. Use a reachable HTTPS origin and keep webhook paths separate from user enrollment. [Self-hosting](SELF_HOSTING.md)

## FlightAware AeroAPI

Mount a stable random secret of at least 32 bytes and set `PLANO_AEROALERTS_KEY_FILE` to its container path. The callback is `POST /v1/webhooks/aeroapi`. FlightAware documents setting a token in the callback URL. Plano accepts `?token=<secret>`. It also accepts HTTPS Basic authentication with username `plano` and the file's value as password. Keep query strings and authorization headers out of logs. [FlightAware callback troubleshooting](https://support.flightaware.com/hc/en-us/articles/32809242173591-Troubleshooting-Common-AeroAPI-Errors)

Create subscriptions in your FlightAware account for the flights and alert events you need. Use the account's callback configuration and consider its effect on any other consumers. Callback delivery and the subsequent authoritative flight fetch can affect provider usage. [AeroAPI alert API](https://www.flightaware.com/commercial/aeroapi/resources/aeroapi-openapi.yml)

For automatic registration in live mode, set `PLANO_AERO_SUBSCRIPTIONS=true`, configure the callback key, and set an HTTPS `PLANO_PUBLIC_URL`. The account needs an existing alert endpoint and permission to create alerts with per-alert targets. Plano uses a per-alert callback URL with a query token and leaves the account-wide endpoint in place. Keep query strings out of the proxy's access logs. The reconciler registers tracked flights with a known operating ICAO designator, an ICAO airport pair, and a future departure-local day within the next 48 hours. The route and day scope can cover more than one flight instance. The callback still uses the provider instance ID to match tracked flights.

Set `PLANO_AERO_SUBSCRIPTIONS_INBOUND=true` to include verified direct inbound legs for tracked departures. This requires `PLANO_AERO_SUBSCRIPTIONS=true` and defaults to `false`. Plano adds an inbound scope only when the cached first leg matches AeroAPI's explicit inbound instance ID and the aircraft, airports, and timing are compatible. When there is room for a new registration, individually tracked scopes come first. Both kinds of scope share the same limits and ownership checks.

`PLANO_AERO_SUBSCRIPTIONS_MAX` defaults to `10` and accepts `1` to `50` managed scopes. `PLANO_AERO_SUBSCRIPTIONS_INTERVAL` defaults to `5m` and accepts `1m` to `1h`. Each pass issues at most eight provider requests and two creates. FlightAware bills alert deliveries. Its `max_weekly=64` setting checks estimated volume when registering the scope. Periodic polling covers same-day flights, unsupported scopes, and provider failures. Plano verifies ownership before retiring a registration. Turning the reconciler off stops provider mutations. Existing date-bounded subscriptions may remain visible in the account. [AeroAPI alert billing and registration](https://www.flightaware.com/commercial/aeroapi/resources/aeroapi-openapi.yml)

Plano creates a scope only while its departure-local day and its date at UTC midnight are both in the future. An existing inbound registration can continue through arrival as long as the onward flight remains eligible. Existing subscriptions and periodic polling can cover inbound legs that become known too late for a new managed registration. [FlightAware same-day alert guidance](https://support.flightaware.com/hc/en-us/articles/32809242173591-Troubleshooting-Common-AeroAPI-Errors)

Plano responds with HTTP `200` after durably accepting an eligible refresh hint. Unknown or provider-blocked flights also receive `200` without a refresh. The provider callback has no issue time or delivery ID, so Plano uses the instance ID to fetch the authoritative FlightAware record. A leased worker writes changes and push jobs transactionally. A callback arriving during the fetch schedules another refresh. Periodic polling remains enabled.

A subscription can also deliver an update for a direct inbound flight that AeroAPI explicitly links to an active tracked departure. Plano uses the fetched inbound record to update the rotation and forecast with current cached airport advisories. It retains the onward flight's observation timestamp.

## FAA SWIM and EUROCONTROL bridges

Set `PLANO_ADVISORY_INGRESS_KEY_FILE` to a different mounted random secret of at least 32 bytes. Authorized operator bridges can send normalized observations to `POST /v1/webhooks/advisory` as `application/json`.

The bridge must set `X-Plano-Timestamp` to the decimal Unix time in seconds and `X-Plano-Signature` to the hexadecimal HMAC-SHA256 of the exact UTF-8 timestamp, a single period, and the exact request body bytes. Sign with the key's UTF-8 bytes after trimming leading and trailing whitespace, as configured by Plano. The request clock must be within five minutes of Plano. A provider observation can be at most 30 seconds ahead of Plano's clock. The observation age limit is six hours for ordinary advisories and 48 hours for eligible FAA airport program definitions described below.

The Plano binary can sign and forward a normalized JSON envelope from stdin.
It can also normalize AIXM 5.1 and 5.1.1 Event NOTAM notifications supplied by
your authorized subscriber and airport program messages from FAA TFMData.

```sh
export PLANO_ADVISORY_RELAY_URL=https://plano.example.net/v1/webhooks/advisory
export PLANO_ADVISORY_INGRESS_KEY_FILE=/run/secrets/plano-advisory-key
plano forward-advisory < normalized.json
plano forward-advisory aixm-notam eurocontrol_nm example-publisher < notification.xml
plano forward-advisory tfms-flow < flow-message.xml
```

Use `faa_swim` or `eurocontrol_nm` for the authorized source. The lowercase
publisher label must identify one publishing namespace and stay consistent
across notices, replacements, and cancellations. The relay reads a qualified
standalone NOTAM or a BasicMessage with embedded Event notifications. It uses
aerodrome notices with one explicit four-letter location. Give a single notice
that lacks `issued` its original RFC 3339 publication timestamp as the final
argument. Provider certificates, subscriptions, and envelope extraction belong
to the operator. [FAA NOTAM modernization brief](https://www.faa.gov/air_traffic/flight_info/aeronav/acf/media/Presentations/5-notam-mod-brief.pdf) · [EUROCONTROL AIXM field mapping](https://swim-eurocontrol.atlassian.net/wiki/spaces/DNOTAM/pages/220791506/NOTAM%2Bmapping%2Bto%2BEvent%2Bextension%2Bobjects)

AIXM input is limited to 1 MiB and a delivery can contain up to 32 distinct
recent revisions. The relay skips older history and preserves each original
publication time. Simple daily UTC schedules can support an airport closure.
Estimated or permanent validity, schedules with exceptions, and qualified
closures stay as airport context. The relay interprets the published `2359`
end-of-day convention as the following midnight. [AIXM schedule rules](https://swim-eurocontrol.atlassian.net/wiki/spaces/ACG/pages/212238473/General%2Bcoding%2Brules%2Bfor%2Bschedules)

The relay exits with `0` after Plano accepts every notice. A future-effective
replacement or cancellation stays pending while the relay sends current
notices and exits with `75`. Use the same input on retry. The server
deduplicates notices it already accepted. Exit `65` means rejected input,
`77` means the relay key was refused, and `78` means configuration or an
unexpected endpoint response.

### FAA TFMData airport programs

Pass one Flow Information XML message of up to 2 MiB from an authorized SWIM
subscriber. The relay supports the documented active Ground Stop and Ground
Delay Program airport definitions, including revisions and refreshes. It uses
the source update timestamp and the published program window. Ground stops
match the origin departure time of flights bound for the named airport and
can still apply during taxi-out. Ground delay programs match the arrival time
at that airport. [FAA Flow Information examples](https://cdm.fly.faa.gov/faq/examples/flow-information-data-feed) · [FAA ground stop rules](https://www.faa.gov/air_traffic/publications/atpubs/foa_html/chap18_section_13.html)

Plano groups revisions by program type and airport because these messages
omit a program identifier. A relevant program can generate an airport alert
and appears in flight details. Before sending an alert, Plano checks the
latest flight against the program window. Flight-specific effects require
membership evidence, so these definitions stay out of flight-delay estimates
and model training.
The relay skips proposals and unrelated message types. An active definition
with its published start and end can stay visible until that end, up to 48
hours after the source update. A repeat of the same revision keeps its original
update time. Forward withdrawals through the normalized bridge using the same
key, such as `tfms/gs/ORD` or `tfms/gdp/ORD`, with a newer `observedAt` and
kind `cancelled`. Submit withdrawals within six hours of their observation.
Plano retains revision ordering for 49 hours to prevent older packets from
restoring a withdrawn or shortened program. Upgrading SQLite extends eligible
saved definitions. The first upgraded startup records a cutoff seven hours
before that startup. Incoming definitions dated at or
before the cutoff have no effect. This prevents older packets from restoring
programs whose withdrawals an earlier release pruned. Fresh databases can
accept eligible definitions within the 48-hour age limit.

The subscriber owns FAA access, broker acknowledgement, and retry. Verify the
message format against the schema for your subscription before forwarding it.
[FAA SWIM access](https://www.faa.gov/air_traffic/technology/swim/products/get_connected) · [TFMData service](https://nsrr.faa.gov/services/tfm-v4/profile)

### Normalized advisories

An example bridge body follows. The dates and airport program are illustrative.

```json
{
  "version": 1,
  "source": "faa_swim",
  "advisory": {
    "id": "example-program-1",
    "airport": "KEWR",
    "kind": "ground_stop",
    "direction": "arrival",
    "reason": "low visibility",
    "observedAt": "2026-11-06T18:10:00Z",
    "startsAt": "2026-11-06T18:00:00Z",
    "endsAt": "2026-11-06T19:30:00Z"
  }
}
```

Valid sources are `faa_swim` and `eurocontrol_nm`. A bridge can include `rawReason`, `area`, and `delayMinutes` when known. The bridge owns provider access, source vocabulary mapping, revision identity, and any area-to-airport matching. Plano uses the airport and operation direction to determine which tracked flights are affected. An area-only observation does not generate flight alerts without that evidence. A materially different revision can produce a new alert in the same program's notification group. Repeated program states are deduplicated for that flight.

When an ICAO replacement or cancellation has its own ID, include `supersedesId` with the earlier notice's normalized ID. For example, a bridge can send `egtt/A0124/26` with `supersedesId` set to `egtt/A0123/26` and `airport` set to `EGLL`. Use the same source and airport spelling for every notice in a chain. Carry the link consistently in polls and callbacks, and retain the original revision's `observedAt` on replay. The bridge resolves the referenced serial in its publishing namespace. Plano uses a recognized raw `NOTAMC` header to classify a cancellation, and the bridge can supply `notam_cancellation` for a normalized record without raw text. [EUROCONTROL operating procedures, section 2.4](https://www.eurocontrol.int/publication/eurocontrol-guidelines-operating-procedures-ais-dynamic-data-opadd)

Plano retires predecessor observations at or before the replacement's observation time once that time is reached. The link stays in SQLite for seven hours after observation, through snapshot removal, replacement expiry, and restarts. A conflicting observation cannot retarget an established successor during that period. Linked poll observations can be up to 30 seconds ahead of the receipt clock. Queued work created before airport scope was stored uses any matching cached advisory or its original expiry.

Before dispatching an advisory or delay-prediction alert, Plano checks the recorded evidence against the latest known eligible revisions. A superseding change or explicit withdrawal drops the queued warning, and an unchanged observation keeps its retry. Plano retains the ordered sequence of accepted future-dated revisions until each can take effect. Queued alerts expire at their evidence deadline. A daily closure alert also requires current source evidence and a pending flight operation in the same occurrence. Other programs that disappear from a poll and jobs queued by older versions generally rely on their original expiry.

Recognized NOTAM text is expanded for passenger context. The original notice is retained with its scope, exceptions, and schedules. Tracked flight details show up to eight recent airport notices that match the departure or arrival window. A successful poll refreshes the cache for up to 30 minutes. Ordinary polled and signed notices expire by six hours after observation or their earlier published end. [FAA movement-area notices](https://www.faa.gov/air_traffic/publications/atpubs/notam_html/chap5_section_1.html) · [FAA international format](https://www.faa.gov/air_traffic/publications/atpubs/notam_html/chap8_section_1.html)

A verified full closure at the departure airport can move Plano's earliest departure estimate to the published reopening time. This requires a fresh notice, a matching airport, and a definite end. Plano reads fixed UTC validity from the FAA date range or ICAO items B and C. A definite published window can supply missing bridge times, and both sources must agree when the bridge supplies a time. For a single daily UTC window (`DLY HHMM-HHMM`), Plano uses the occurrence that overlaps the flight, including overnight windows. The occurrence is clipped to the published dates. More complex schedules, partial closures, estimated ends, and conflicting times remain airport context. Use the airline for current flight status. [FAA NOTAM validity and schedules](https://www.faa.gov/air_traffic/publications/atpubs/notam_html/chap4_section_2.html) · [EUROCONTROL operating procedures](https://www.eurocontrol.int/publication/eurocontrol-guidelines-operating-procedures-ais-dynamic-data-opadd)

## Timing

The outbox wakes as soon as its transaction commits. It keeps independent sends in flight and checks for persisted retries every 200 ms. Provider fetch time, APNs or FCM acceptance, delivery to a device, and messaging-only Wi-Fi behavior are separate stages. The fixture tests verify callback-to-dispatch behavior using local provider substitutes. Set `PLANO_LOG_LEVEL=debug` to log provider acceptance time and observation-to-send timing without including device tokens. Live latency and device delivery must be measured with configured providers and signed apps.
