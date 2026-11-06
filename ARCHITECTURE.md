# Plano architecture and Flighty evidence

## Flighty investigation

REA 6.5.0 returned a target-free native session on Linux. Its macOS provider reported an unsupported host, and no Ghidra installation was configured. The local artifact search covered the project parent, Downloads and temporary directories to depth four. It found no Flighty IPA, app bundle or capture. No native analysis target was opened.

| Question | Publicly documented behavior | Missing evidence |
| --- | --- | --- |
| Aviation data | Flighty names FAA, EUROCONTROL and aviation authority advisories. It tracks an assigned inbound aircraft for up to 25 hours and presents its prediction alongside airline times. [App Store](https://apps.apple.com/us/app/flighty-live-flight-tracker/id1358823008) · [Inbound tracking](https://flighty.com/help/where-is-my-plane) · [Delay predictions](https://flighty.com/help/delay-predictions) | Vendor contracts, actual feed identifiers, ingest topology and model parameters. |
| Notifications | Flighty documents aircraft, gate, disruption, diversion, taxi, takeoff, landing, baggage and check-in alerts. It documents updates on messaging-only onboard Wi-Fi. [Notification settings](https://flighty.com/help/flighty-notifications) · [Offline mode](https://flighty.com/help/offline-mode) | Internal event transitions, suppression thresholds, upstream processing times and device delivery traces. |
| Live Activities | The documented Lock Screen and Dynamic Island surfaces include countdown, gate and arrival progress. Flighty documents its supported platforms separately. [Live Activities](https://flighty.com/help/live-activities-widgets) · [Supported platforms](https://flighty.com/help/supported-platforms) | Signed extension metadata, Swift type names, content-state fields and push frames. |
| Trips and history | Flighty documents calendar, TripIt and email import, flight history and friend tracking. [Flight management](https://flighty.com/help/managing-my-flights) · [Passport](https://flighty.com/help/passport) · [Friends](https://flighty.com/help/flighty-friends) | Private provider interfaces, synchronization protocol, account graph and personal-data retention. |

Plano uses original interfaces and an independent implementation. The Flighty feature descriptions establish product behavior to reproduce. Native and server internals remain unverified.

## Plano data flow

```text
AeroAPI search / active-flight polling    FAA NAS / operator advisory gateways
                         │                    │
                         └─────────┬──────────┘
                                   ▼
             Flight snapshot + track + inbound rotation + advisory context
                                   │
                                   ▼
             SQLite transaction: snapshot → typed events → push outbox
                         │                     │
                         ▼                     ▼
          Go HTTP API / share page      APNs and FCM HTTP v1
                         │                     │
                         ▼                     ▼
             Expo React Native UI        Native OS surfaces
```

- Flight IDs use provider identity when available. The schedule adapter retains synthetic schedule identity when a matching live record appears. Times use ISO 8601 on HTTP and epoch seconds for push. Departure-date search uses the origin's local date where known.
- OOOI observations represent gate-out, wheels-off, wheels-on and gate-in separately. Status can also express boarding, cancellation and diversion. A later snapshot can revise an estimate. A previous observation cannot overwrite a newer snapshot.
- SQLite stores authenticated installations, tracked flights, share capabilities, device registrations and the push outbox. Device fanout and event writes share a transaction. A leased push job can be retried after a worker restart. Delivery is at least once, subject to expiry, provider policy and retry limits.
- User-visible changes include tail assignment, departure and arrival gate, baggage, significant ETD/ETA shifts, OOOI and matching advisories. Check-in is a 24-hour reminder with airline availability unverified. The operational departure estimate uses aircraft turnaround and applicable advisory constraints. Its reasons and official times stay separate.
- A push message carries enough state to render the flight without an immediate API fetch. The push client enforces a 2 KiB request envelope. The internal Live Activity contract is `PlanoActivityAttributes` plus `flightId`, `flightNumber`, `status`, origin and destination codes, gate and terminal fields, optional seat, and departure, arrival and observation epochs.
- The default active-flight polling interval is 60 seconds. The 200 ms outbox poll governs pickup of a ready local job. Provider acceptance, device arrival, end-to-end alert latency and behavior on specific onboard networks require separate measurement.
- Every product route is available without a billing or entitlement check. External feed access, Apple signing and cloud push credentials belong to the instance operator. The self-host service can also run against explicitly labeled synthetic data for local evaluation. [Self-hosting](docs/SELF_HOSTING.md)

## Verification still required for full parity

| Area | Required evidence |
| --- | --- |
| Flighty internals | Lawfully available native package or runtime capture, with REA target identity and artifact or trace evidence for each claimed subsystem. |
| Push and native activities | Signed iOS and Android builds, both live provider credentials, platform-version and lifecycle tests, device receipt timestamps, and tests on constrained onboard Wi-Fi. |
| Aviation ingestion | Approved live source accounts and observed coverage for terminal, baggage, boarding, historical tracks, raw SWIM and EUROCONTROL data and aircraft registry enrichment. |
| Prediction | Versioned training observations, holdout/backtest metrics, and validation of turnaround, weather, ATC and NOTAM attribution. The current estimate is rule-based. |
| Connections and sync | Verified airport-specific transfer/navigation and gate predictions, calendar background refresh, inbound email transport, TripIt account integration and authorized PNR lookup. |
| Passport and Friends | Complete history and coordinate coverage, shareable card verification, and a multi-user friend graph if required beyond per-flight links. |
| Self-host deployment | Container start with mounted storage, restart persistence, phone-to-server TLS, secret mounts, recovery and operation under expected load. |
