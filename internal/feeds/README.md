# Aviation feed contracts

Plano's adapters record provenance on flight snapshots, observed positions, and advisories. Provider coverage varies by carrier, airport, region, and subscription. Missing data stays absent.

## FlightAware AeroAPI

`AeroAPI` uses a server-side key. It reads recent and historical flights, future schedules, airport metadata, and observed tracks from the provider. Airport timezone determines the requested calendar date. Future timetable entries without a FlightAware ID receive a Plano ID that is retained when the live flight can be matched by route, operating designator, and departure time. FlightAware's track endpoint supplies both ADS-B and other observation types, which Plano labels separately. [AeroAPI OpenAPI 4.17.1](https://www.flightaware.com/commercial/aeroapi/resources/aeroapi-openapi.yml)

Rotations return the immediate inbound first, then the earlier legs in the
25-hour window. An explicit provider link takes precedence when aircraft,
airport, and timing evidence agree. Tail-only matching uses movement order.
The refresh worker retains cached history on a failed lookup with no verified
legs. A verified partial result replaces the older chain, and an aircraft
assignment change clears any history that could belong to the previous plane.

The API requires an eligible account and plan. Check which endpoints, request volumes, storage, and public redistribution your agreement covers before enabling live data for other users. [AeroAPI license](https://www.flightaware.com/commercial/aeroapi/AeroAPI_Standard_License.pdf)

## Public FAA NAS Status

`FAANAS` reads the public airport status XML with no account. It normalizes airport ground stops, ground delay programs, arrival and departure delays, airport restrictions, airspace flow programs, and collaborative trajectory options programs. Airspace-only notices need route correlation before they can be attributed to a particular flight. Ground stop and delay programs for an airport generally apply to flights destined there, with details and exceptions that can vary by program. [FAA NAS Status](https://nasstatus.faa.gov/) · [FAA airport status XML](https://nasstatus.faa.gov/api/airport-status-information) · [FAA advisory guidance](https://www.fly.faa.gov/ais/Help/ais_message_help.html)

Conditional NOTAM summaries retain the original text under `rawReason`. The English summary covers a limited set of common airport closure patterns. It does not resolve every exception or recurring schedule.

## FAA SWIM and EUROCONTROL

The optional `AdvisoryGateway` consumes a normalized HTTP snapshot from an operator-managed bridge. FAA SWIM Cloud Distribution Service requires a service access agreement and subscription. EUROCONTROL NM B2B is restricted by stakeholder eligibility, usage conditions, and TLS client certificates, with SOAP or plain XML requests and AMQP subscriptions. The bridge owns the upstream connection and schema translation. [FAA SWIFT and SCDS access](https://support.swim.faa.gov/hc/en-us) · [EUROCONTROL NM B2B access and protocols](https://www.eurocontrol.int/service/network-manager-business-business-b2b-web-services)

The gateway endpoint must use HTTPS or a loopback IP over HTTP. Plano sends `GET`, `Accept: application/json`, and an optional bearer credential. A successful response supplies a complete snapshot with a version and an advisory array. Use an empty array to report an observed state with no active notices.

```json
{
  "version": 1,
  "advisories": [
    {
      "id": "regulation-fixture",
      "airport": "EGLL",
      "kind": "ground_delay",
      "direction": "arrival",
      "reason": "Arrival capacity reduced by low visibility",
      "observedAt": "2026-11-05T17:00:00Z",
      "startsAt": "2026-11-05T16:30:00Z",
      "endsAt": "2026-11-05T18:30:00Z",
      "delayMinutes": 35
    }
  ]
}
```

Use a stable source-native ID, a lowercase kind, an `airport` or `area` identifier, and `observedAt` in RFC 3339. `direction`, `startsAt`, `endsAt`, `delayMinutes`, and `rawReason` are optional. The adapter sets `source` from its configuration. Prefer `ground_stop`, `ground_delay`, `arrival_delay`, `departure_delay`, `airport_closure`, and `airport_restriction` when they fit the provider notice. An airport aggregate delay is an advisory signal and can differ from the delay assigned to an individual flight.

## Operator ADS-B and FAA registry

`ADSBReceiver` reads a private operator receiver's `readsb` `aircraft.json` over HTTPS or loopback HTTP. It selects fresh measured transponder positions for active flights with an established aircraft identity. Coverage is limited to the receiver's reception area and any authorized network supplying it. Configure the receiver's aircraft database and access policies to respect aircraft privacy restrictions. [readsb JSON output](https://github.com/wiedehopf/readsb/blob/dev/README-json.md)

Plano can read the FAA release ZIP directly from an operator-supplied file. It streams `MASTER.txt` and joins its manufacturer and model code to `ACFTREF.txt`. It keeps US tail numbers, transponder identifiers, manufacture years, and model names. An extracted `MASTER.txt` supplies identity and age. Aircraft age is the difference between calendar years. The current registry enriches near-term flights with an assigned tail. Saved flight details can carry earlier model and age observations forward for the same verified flight instance and tail. [FAA registry download](https://www.faa.gov/licenses_certificates/aircraft_certification/aircraft_registry/releasable_aircraft_download/index.cfm) · [FAA file layouts](https://registry.faa.gov/database/ardata.pdf)
