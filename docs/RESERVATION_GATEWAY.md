# Reservation lookup gateway

An operator can connect Plano to a reservation system through a bridge they control. The bridge operator needs licensed access to the applicable airline or reservation system and must verify that the lookup is authorized for the reservation. Plano supplies this HTTP contract. The operator supplies the bridge and manages its upstream access.

Set `PLANO_RESERVATION_GATEWAY_URL` to the fixed bridge endpoint and `PLANO_RESERVATION_GATEWAY_TOKEN_FILE` to a private bearer credential file readable by the Plano process. `PLANO_RESERVATION_GATEWAY_TOKEN` also accepts the credential directly. Use HTTPS for the bridge or literal loopback HTTP when the processes share a network namespace. The endpoint URL contains no credentials, query string, or fragment. Plano does not follow redirects. Keep authorization headers, request bodies, and response bodies out of the bridge and proxy logs. Apply lookup limits at the bridge to prevent repeated guesses.

Plano sends a `POST` with an `Authorization` bearer header and a JSON body. The following request uses synthetic booking details.

```json
{
  "version": 1,
  "carrier": "UA",
  "locator": "A1B2C3",
  "surname": "Example"
}
```

`carrier` is an uppercase two-character IATA designator or three-letter ICAO designator. `locator` is a six-character alphanumeric booking reference. Plano trims the surname and preserves its case. The bridge must perform the authorization and matching required by its reservation provider before returning flight legs.

Return `200` with `Content-Type: application/json` and the following shape for an authorized match. Supply the date at the departure airport, in `YYYY-MM-DD` form, and use uppercase flight designators without spaces. The example legs are synthetic.

```json
{
  "version": 1,
  "authorized": true,
  "legs": [
    {"number": "UA123", "date": "2026-12-01", "origin": "SFO", "destination": "DEN", "departure": "2026-12-01T17:30:00-08:00"},
    {"number": "UA456", "date": "2026-12-05"}
  ]
}
```

The optional `origin` and `destination` fields accept uppercase IATA or
ICAO airport codes. The optional `departure` field is the scheduled gate
departure as an RFC 3339 instant. Supply these fields when a flight number
serves multiple legs on the same day. The `date` still uses the departure
airport's calendar day, which can differ from the UTC date of the instant.
Plano filters by the supplied airports before matching the departure time.
It prefers an exact time and accepts a unique match within 45 minutes
when no exact match is available. Legs that remain ambiguous require
manual review. [RFC 3339 timestamp format](https://www.rfc-editor.org/rfc/rfc3339.html#section-5.6)

Return up to eight legs and 16 KiB of JSON within six seconds. The response may contain only the fields above. For an unauthorized or missing reservation, return `404` or return `200` with `authorized` set to `false` and an empty `legs` array. Plano treats other statuses and malformed responses as provider failures. It omits the bridge response body from errors.
