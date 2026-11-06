# Event-driven aviation updates

Plano accepts two optional inbound callback types in live mode. Both use the same SQLite event and push pipeline as periodic ingestion. Use a reachable HTTPS origin and keep webhook paths separate from user enrollment. [Self-hosting](SELF_HOSTING.md)

## FlightAware AeroAPI

Mount a stable random secret of at least 32 bytes and set `PLANO_AEROALERTS_KEY_FILE` to its container path. The callback is `POST /v1/webhooks/aeroapi`. FlightAware documents setting a token in the callback URL. Plano accepts `?token=<secret>`. It also accepts HTTPS Basic authentication with username `plano` and the file's value as password. Keep query strings and authorization headers out of logs. [FlightAware callback troubleshooting](https://support.flightaware.com/hc/en-us/articles/32809242173591-Troubleshooting-Common-AeroAPI-Errors)

Create subscriptions in your FlightAware account for the flights and alert events you need. Use the account's callback configuration and consider its effect on any other consumers. Callback delivery and the subsequent authoritative flight fetch can affect provider usage. [AeroAPI alert API](https://www.flightaware.com/commercial/aeroapi/resources/aeroapi-openapi.yml)

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

Valid sources are `faa_swim` and `eurocontrol_nm`. A bridge can include `rawReason`, `area`, and `delayMinutes` when known. The bridge owns provider access, source vocabulary mapping, revision identity, and any area-to-airport matching. Plano uses the airport and operation direction to determine which tracked flights are affected. An area-only observation does not generate flight alerts without that evidence. A newer observation can revise an existing program. Plano sends one initial alert per source program and tracked flight. Later revisions feed periodic delay estimates, and a replay cannot enqueue the initial alert twice.

## Timing

The outbox wakes as soon as its transaction commits. It keeps independent sends in flight and checks for persisted retries every 200 ms. Provider fetch time, APNs or FCM acceptance, delivery to a device, and messaging-only Wi-Fi behavior are separate stages. The fixture tests verify callback-to-dispatch behavior using local provider substitutes. Set `PLANO_LOG_LEVEL=debug` to log provider acceptance time and observation-to-send timing without including device tokens. Live latency and device delivery must be measured with configured providers and signed apps.
