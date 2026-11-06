# Event-driven aviation updates

Plano accepts optional live flight and advisory callbacks. They use the same SQLite event and push pipeline as periodic ingestion. Use a reachable HTTPS origin and keep webhook paths separate from user enrollment. [Self-hosting](SELF_HOSTING.md)

## FlightAware AeroAPI

Mount a stable random secret of at least 32 bytes and set `PLANO_AEROALERTS_KEY_FILE` to its container path. The callback is `POST /v1/webhooks/aeroapi`. FlightAware documents setting a token in the callback URL. Plano accepts `?token=<secret>`. It also accepts HTTPS Basic authentication with username `plano` and the file's value as password. Keep query strings and authorization headers out of logs. [FlightAware callback troubleshooting](https://support.flightaware.com/hc/en-us/articles/32809242173591-Troubleshooting-Common-AeroAPI-Errors)

Create subscriptions in your FlightAware account for the flights and alert events you need. Use the account's callback configuration and consider its effect on any other consumers. Callback delivery and the subsequent authoritative flight fetch can affect provider usage. [AeroAPI alert API](https://www.flightaware.com/commercial/aeroapi/resources/aeroapi-openapi.yml)

For automatic registration in live mode, set `PLANO_AERO_SUBSCRIPTIONS=true`, configure the callback key, and set an HTTPS `PLANO_PUBLIC_URL`. The account needs an existing alert endpoint and permission to create alerts with per-alert targets. Plano uses a per-alert callback URL with a query token and leaves the account-wide endpoint in place. Keep query strings out of the proxy's access logs. The reconciler registers tracked flights with a known operating ICAO designator, an ICAO airport pair, and a future departure-local day within the next 48 hours. The route and day scope can cover more than one flight instance. The callback still uses the provider instance ID to match tracked flights.

Set `PLANO_AERO_SUBSCRIPTIONS_INBOUND=true` to include verified direct inbound legs for tracked departures. This requires `PLANO_AERO_SUBSCRIPTIONS=true` and defaults to `false`. Plano adds an inbound scope only when the cached first leg matches AeroAPI's explicit inbound instance ID and the aircraft, airports, and timing are compatible. When there is room for a new registration, individually tracked scopes come first. Both kinds of scope share the same limits and ownership checks.

`PLANO_AERO_SUBSCRIPTIONS_MAX` defaults to `10` and accepts `1` to `50` managed scopes. `PLANO_AERO_SUBSCRIPTIONS_INTERVAL` defaults to `5m` and accepts `1m` to `1h`. Each pass issues at most eight provider requests and two creates. FlightAware bills alert deliveries. Its `max_weekly=64` setting checks estimated volume when registering the scope. Periodic polling covers same-day flights, unsupported scopes, and provider failures. Plano verifies ownership before retiring a registration. Turning the reconciler off stops provider mutations. Existing date-bounded subscriptions may remain visible in the account. [AeroAPI alert billing and registration](https://www.flightaware.com/commercial/aeroapi/resources/aeroapi-openapi.yml)

Plano creates a scope only while its departure-local day and its date at UTC midnight are both in the future. An existing inbound registration can continue through arrival as long as the onward flight remains eligible. Existing subscriptions and periodic polling can cover inbound legs that become known too late for a new managed registration. [FlightAware same-day alert guidance](https://support.flightaware.com/hc/en-us/articles/32809242173591-Troubleshooting-Common-AeroAPI-Errors)

Plano responds with HTTP `200` after durably accepting an eligible refresh hint. Unknown or provider-blocked flights also receive `200` without a refresh. The provider callback has no issue time or delivery ID, so Plano uses the instance ID to fetch the authoritative FlightAware record. A leased worker writes changes and push jobs transactionally. A callback arriving during the fetch schedules another refresh. Periodic polling remains enabled.

A subscription can also deliver an update for a direct inbound flight that AeroAPI explicitly links to an active tracked departure. Plano uses the fetched inbound record to update the rotation and forecast with current cached airport advisories. It retains the onward flight's observation timestamp.

## Signed flight snapshots

If an authorized operator bridge already receives complete flight snapshots,
send them directly to `POST /v1/webhooks/flight-observation`. This path applies
each snapshot to matching tracked flights or their verified direct inbound
legs without making another AeroAPI request. It uses the same provider flight
ID, event transitions, and durable push outbox as the ordinary AeroAPI callback.
AeroAPI supplies schedule search and periodic refresh supplies radar tracks.

Mount a separate random signing key of at least 32 bytes and set
`PLANO_FLIGHT_OBSERVATION_KEY_FILE` to its container path. `plano check-config`
rejects a key shared with either AeroAPI callbacks or advisory ingress. The
endpoint is available in live mode. The relay reads one `application/json`
envelope of up to 64 KiB from standard input.

```sh
export PLANO_FLIGHT_OBSERVATION_RELAY_URL=https://plano.example.net/v1/webhooks/flight-observation
export PLANO_FLIGHT_OBSERVATION_KEY_FILE=/run/secrets/plano-flight-observation-key
plano forward-flight-observation < snapshot.json
```

The envelope has `version: 1`, `observedAt` set to the original snapshot
capture time in RFC 3339, and `flight` set to a full AeroAPI `BaseFlight`
object for a commercial flight with an exact `fa_flight_id`. Include nullable
gate, terminal, baggage, assignment, and timing fields explicitly. An explicit
null can withdraw an earlier value. For a diversion that shares its ID with a continuation leg,
send the original flight record. Plano checks the origin for directly tracked
departures. The normal AeroAPI alert's partial `flight` object and raw
Firehose messages require a bridge to produce a complete snapshot before
delivery. [AeroAPI flight object and callback schemas](https://www.flightaware.com/commercial/aeroapi/resources/aeroapi-openapi.yml)

A bridge sending HTTP directly signs the decimal Unix seconds from
`X-Plano-Timestamp`, one period, and the exact JSON bytes with HMAC-SHA256. Send
the signature as hexadecimal in `X-Plano-Signature`. The request timestamp
must be within five minutes of the server clock. The snapshot capture time
must be within the preceding five minutes, and the scheduled departure must
be between 36 hours ago and 48 hours ahead. Preserve the original capture time
on retries. A `204` acknowledges receipt, including an older or duplicate
snapshot or one with no eligible flight. Each accepted request returns the
same empty response.

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

### Flight-specific takeoff slots

An authorized bridge can report an FAA EDCT or EUROCONTROL CTOT with
`POST /v1/webhooks/atfm-slot`. The live-mode endpoint uses the advisory signing
key and the same signature headers. Use one authoritative bridge per source.

```sh
export PLANO_ATFM_SLOT_RELAY_URL=https://plano.example.net/v1/webhooks/atfm-slot
export PLANO_ADVISORY_INGRESS_KEY_FILE=/run/secrets/plano-advisory-key
plano forward-atfm-slot < slot.json
```

Send one normalized JSON envelope of up to 4 KiB. The bridge matches its
source flight record to an exact AeroAPI `fa_flight_id` using verified flight
identity. Use the tracked flight's two ICAO airports and airline scheduled
departure instant. All times use RFC 3339, including the resolved UTC date of
the slot if the upstream field supplies only a time of day. [FAA flight identity](https://cdm.fly.faa.gov/faq/required-elements-tfmdata-flight-request) · [EUROCONTROL ADEXP 4.0](https://www.eurocontrol.int/sites/default/files/2026-04/eurocontrol-adexp-specification-4-0.pdf)

```json
{
  "version": 1,
  "source": "faa_swim",
  "providerFlightId": "example-UAL88-provider-id",
  "originIcao": "KSFO",
  "destinationIcao": "KEWR",
  "scheduledDeparture": "2026-11-06T18:00:00Z",
  "observedAt": "2026-11-06T17:50:00Z",
  "status": "assigned",
  "takeoffAt": "2026-11-06T18:45:00Z",
  "reason": "Arrival capacity reduced by thunderstorms"
}
```

Use `eurocontrol_nm` for a CTOT. For a revision, send the new assignment
with its observation time. For a withdrawal, send `status` as `withdrawn`
and omit `takeoffAt`. An offer of an improved EUROCONTROL slot is still a
proposal. Supply `reason` only when the upstream source attributes it to the
flight. The relay accepts this normalized JSON from an authorized bridge.
[FAA EDCT](https://www.faa.gov/air_traffic/publications/aim_html/chap5_section_2.html) · [EUROCONTROL slot messages](https://www.eurocontrol.int/archive_download/all/node/10578)

Preserve the original `observedAt` on retries. It must be within the preceding
30 minutes, and the flight's scheduled departure must be between 12 hours ago
and 24 hours ahead. HTTP `204` acknowledges receipt, including duplicates and
updates without an eligible tracked match. Plano shows current assignments to
installations tracking the verified flight and can send them a private alert.
The displayed slot expires after 30 minutes or the end of the assigned slot
window, whichever is earlier. It also disappears after takeoff or a withdrawal.
The slot describes wheels-off timing. Airline gate times and Plano's delay
estimates remain separate.

#### FAA TFMData FlightControl XML

An authorized subscriber can relay one supported legacy TFMData
`FlightControl` XML message of up to 64 KiB. The adapter reads the controlled
departure time `ctd` for GDP, AFP, and CTOP assignments. An explicit
cancellation of the flight's control assignment withdraws its runway time.
The relay uses the original
`sourceTimeStamp` to order revisions. [FAA SWIFT field mapping, p. 82](https://www.faa.gov/sites/faa.gov/files/2018-May-22-SWIFT-6-Meeting-Presentation.pdf.pdf) · [NASA ATD-2 glossary, p. 67](https://aviationsystems.arc.nasa.gov/publications/atd2/tech-transfers/1_High-Level_and_Project_Documents/1.2-01%20ATD2_Glossary_v4.0_20210519.pdf) · [FIXM US dictionary §3.34](https://fixm.aero/releases/US_Ext-3.1/FIXM_US_Extension_v3_1_Data_Dictionary.pdf)

```sh
plano forward-atfm-slot tfms-flight-control flight-binding.json < flight-control.xml
```

The binding is a JSON file of up to 4 KiB. Match the FAA flight reference,
operating aircraft identification, FAA airport codes, and initial gate
departure time (`igtd`) independently to the AeroAPI flight. Supply the
tracked flight's scheduled departure as a separate RFC 3339 instant. Supply
the raw FAA airport identifiers in `sourceOrigin` and `sourceDestination`
even when they equal the ICAO codes. The relay compares the message header and
qualified flight identity with the binding. The server also checks the exact
tracked flight, ICAO route, and scheduled departure. [FAA flight matching](https://cdm.fly.faa.gov/faq/required-elements-tfmdata-flight-request) · [FAA Flight Data envelope examples](https://cdm.fly.faa.gov/faq/submitting-terminal-flight-data-tfms)

```json
{
  "version": 1,
  "providerFlightId": "example-UAL88-provider-id",
  "flightRef": "1234567",
  "aircraftId": "UAL88",
  "igtd": "2026-11-06T18:00:00Z",
  "sourceOrigin": "SFO",
  "sourceDestination": "EWR",
  "originIcao": "KSFO",
  "destinationIcao": "KEWR",
  "scheduledDeparture": "2026-11-06T18:00:00Z"
}
```

Extract a single `tfmDataService` / `fltdOutput` / `fltdMessage` from the
subscriber stream. The supported message declares `FlightControl`,
`TMI_UPDATE`, and `TFMS` as its type, trigger, and source. Restricted records
produce no signed message. The subscriber supplies approved data and applies
the feed's distribution and LADD restrictions. Validate the payload against
the schema for your subscription. Current TFMData v4 wire compatibility
requires provider fixtures. [FAA SWIM access](https://www.faa.gov/air_traffic/technology/swim/products/get_connected) · [FAA LADD](https://www.faa.gov/pilots/ladd)

#### EUROCONTROL ADEXP messages

An authorized subscriber can pass one ADEXP slot message, up to 16 KiB, to
the same relay. `SAM` assigns a slot from `CTOT`, `SRM` revises it from
`NEWCTOT`, and `SLC` withdraws it. Pass an independently verified AeroAPI
flight ID, matching `IFPLID`, and the airline's scheduled departure instant.
The relay checks the `IFPLID` in the message. The server checks the route and
scheduled departure against the tracked flight. `EOBD` and `EOBT` describe
estimated off-block timing in the flight plan. [ATFCM Users Manual §8.2.3 and Annex 1](https://www.eurocontrol.int/archive_download/all/node/10578)

```sh
plano forward-atfm-slot eurocontrol-adexp "$fa_flight_id" "$ifplid" \
  "$scheduled_departure" "$observed_at" "$takeoff_date" < slot.adexp
```

Supply `scheduled_departure` and the original source observation or receipt
time in RFC 3339. If the message contains `TIMESTAMP`, pass `-` for the
observation argument or supply an instant that agrees with it. Supply a
verified `takeoff_date` in UTC as `YYYY-MM-DD` when the assignment omits
`CTOD`. If both dates are supplied they must agree. Omit the final argument
for an `SLC`. Keep the original observation time and mapping when retrying.
[ADEXP 4.0 §5 and Annex A](https://www.eurocontrol.int/sites/default/files/2026-04/eurocontrol-adexp-specification-4-0.pdf)

The relay translates supported `REGCAUSE` combinations for weather, air
traffic control capacity, and airport capacity. Operator comments and other
raw message fields stay out of the signed envelope. The subscriber supplies
upstream access, message framing, and flight identity mapping.
[EUROCONTROL regulation reasons, Annex D](https://www.eurocontrol.int/archive_download/all/node/10578)

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
update time. The same relay accepts `GADV` general advisories carrying the
structured actual ATCSCC ground stop or ground delay program cancellation.
Plano applies each cancellation to operations in its published cancellation
interval when it is newer than the program definition. Proposals and free-form
facility releases are outside this path. [FAA GS cancellation example](https://www.fly.faa.gov/adv/adv_otherdis?adv_date=09032026&advn=59) · [FAA GDP cancellation example](https://www.fly.faa.gov/adv/adv_otherdis?adv_date=11022026&advn=11)

An operator bridge can still withdraw an entire definition using its key,
such as `tfms/gs/ORD` or `tfms/gdp/ORD`, with a newer `observedAt` and
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
