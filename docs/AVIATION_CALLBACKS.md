# Event-driven aviation updates

Plano accepts two optional inbound callback types in live mode. Both use the same SQLite event and push pipeline as periodic ingestion. Use a reachable HTTPS origin and keep webhook paths separate from user enrollment. [Self-hosting](SELF_HOSTING.md)

## FlightAware AeroAPI

Mount a stable random secret of at least 32 bytes and set `PLANO_AEROALERTS_KEY_FILE` to its container path. The callback is `POST /v1/webhooks/aeroapi`. FlightAware documents setting a token in the callback URL. Plano accepts `?token=<secret>`. It also accepts HTTPS Basic authentication with username `plano` and the file's value as password. Keep query strings and authorization headers out of logs. [FlightAware callback troubleshooting](https://support.flightaware.com/hc/en-us/articles/32809242173591-Troubleshooting-Common-AeroAPI-Errors)

Create subscriptions in your FlightAware account for the flights and alert events you need. Use the account's callback configuration and consider its effect on any other consumers. Callback delivery and the subsequent authoritative flight fetch can affect provider usage. [AeroAPI alert API](https://www.flightaware.com/commercial/aeroapi/resources/aeroapi-openapi.yml)

For automatic registration in live mode, set `PLANO_AERO_SUBSCRIPTIONS=true`, configure the callback key, and set an HTTPS `PLANO_PUBLIC_URL`. The account needs an existing alert endpoint and permission to create alerts with per-alert targets. Plano uses a per-alert callback URL with a query token and leaves the account-wide endpoint in place. Keep query strings out of the proxy's access logs. The reconciler registers tracked flights with a known operating ICAO designator, an ICAO airport pair, and a future departure-local day within the next 48 hours. The route and day scope can cover more than one flight instance. The callback still uses the provider instance ID to match tracked flights.

`PLANO_AERO_SUBSCRIPTIONS_MAX` defaults to `10` and accepts `1` to `50` managed scopes. `PLANO_AERO_SUBSCRIPTIONS_INTERVAL` defaults to `5m` and accepts `1m` to `1h`. Each pass issues at most eight provider requests and two creates. Registrations use FlightAware's `max_weekly=64` estimate gate. Provider terms, quotas, and actual delivery still apply. Periodic polling covers same-day flights, unsupported scopes, and provider failures. Plano verifies ownership before retiring a registration. Turning the reconciler off stops provider mutations. Existing date-bounded subscriptions may remain visible in the account.

Plano responds with HTTP `200` after durably accepting a tracked flight's refresh hint. Untracked or provider-blocked flights also receive `200` without a refresh. The provider callback has no issue time or delivery ID, so its flight fields do not update the timeline directly. A leased worker fetches the current FlightAware record by instance ID, then writes changes and push jobs transactionally. A callback arriving during the fetch schedules another refresh. Periodic polling remains enabled.

## FAA SWIM and EUROCONTROL bridges

Set `PLANO_ADVISORY_INGRESS_KEY_FILE` to a different mounted random secret of at least 32 bytes. Authorized operator bridges can send normalized observations to `POST /v1/webhooks/advisory` as `application/json`.

The bridge must set `X-Plano-Timestamp` to the decimal Unix time in seconds and `X-Plano-Signature` to the hexadecimal HMAC-SHA256 of the exact UTF-8 timestamp, a single period, and the exact request body bytes. Sign with the key's UTF-8 bytes after trimming leading and trailing whitespace, as configured by Plano. The request clock must be within five minutes of Plano. A provider observation must be within the past six hours and at most 30 seconds ahead of Plano's clock.

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

Before dispatching an advisory or delay-prediction alert, Plano checks the recorded evidence against the latest known eligible revisions. A superseding change or explicit withdrawal drops the queued warning, and an unchanged observation keeps its retry. Plano retains the ordered sequence of accepted future-dated revisions until each can take effect. Queued alerts expire at their evidence deadline. A program that simply disappears from a poll relies on that deadline. Jobs queued by older versions also rely on their original expiry.

Recognized NOTAM text is expanded for passenger context. The original notice is retained with its scope, exceptions, and schedules. Tracked flight details show up to eight recent airport notices that match the departure or arrival window. A successful poll refreshes the cache for up to 30 minutes. Both polled and signed notices expire by six hours after observation or their earlier published end. [FAA movement-area notices](https://www.faa.gov/air_traffic/publications/atpubs/notam_html/chap5_section_1.html) · [FAA international format](https://www.faa.gov/air_traffic/publications/atpubs/notam_html/chap8_section_1.html)

A verified full closure at the departure airport can move Plano's earliest departure estimate to the published end of the closure. This requires a fresh notice, a matching airport, and a definite end. Plano reads fixed UTC validity from the FAA date range or ICAO items B and C. A definite published window can supply missing bridge times, and both sources must agree when the bridge supplies a time. Partial, recurring, estimated, and conflicting closures appear as airport context. Use the airline for current flight status. [FAA NOTAM validity and schedules](https://www.faa.gov/air_traffic/publications/atpubs/Notam_html/chap4_section_2.html) · [EUROCONTROL operating procedures](https://www.eurocontrol.int/publication/eurocontrol-guidelines-operating-procedures-ais-dynamic-data-opadd)

## Timing

The outbox wakes as soon as its transaction commits. It keeps independent sends in flight and checks for persisted retries every 200 ms. Provider fetch time, APNs or FCM acceptance, delivery to a device, and messaging-only Wi-Fi behavior are separate stages. The fixture tests verify callback-to-dispatch behavior using local provider substitutes. Set `PLANO_LOG_LEVEL=debug` to log provider acceptance time and observation-to-send timing without including device tokens. Live latency and device delivery must be measured with configured providers and signed apps.
