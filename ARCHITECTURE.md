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
AeroAPI search / polling / callbacks     FAA NAS / signed advisory bridges
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
- User-visible changes include tail assignment, departure and arrival gate, baggage, significant ETD/ETA shifts, OOOI and matching advisories. Check-in is a 24-hour reminder with airline availability unverified. The operational departure estimate uses aircraft turnaround and applicable advisory constraints. Operators can train and deploy a local ridge-regression model from time-stamped observations. Its inputs, contributions and official flight times stay separate.
- The optional METAR worker fetches small batches of public station observations for imminent tracked operations. The flight API returns recent adverse conditions separately from delay reasons, model inputs, and official airport advisories. [Aviation Weather Center data API](https://aviationweather.gov/data/api/)
- A push message carries enough state to render the flight without an immediate API fetch. The push client enforces a 2 KiB request envelope. The internal Live Activity contract is `PlanoActivityAttributes` plus `flightId`, `flightNumber`, `status`, origin and destination codes, gate and terminal fields, optional seat, and departure, arrival and observation epochs.
- The iOS Lock Screen and expanded Dynamic Island show gate, terminal, phase, and seat. Compact uses a system-rendered countdown when space allows. Minimal shows a phase icon. Stale activities show an awaiting-update cue and suppress progress. [WidgetKit dynamic dates](https://developer.apple.com/documentation/widgetkit/displaying-dynamic-dates)
- The iOS coordinator retries unfinished token uploads on foreground. It stops per-activity observation at the end of an activity and fences uploads when the server or installation credential changes. Slot-aware registration replaces the previous token for the same installation and activity in one SQLite transaction. [ActivityKit push notifications](https://developer.apple.com/documentation/activitykit/starting-and-updating-live-activities-with-activitykit-push-notifications)
- The default active-flight polling interval is 60 seconds. FlightAware callbacks can wake a durable targeted refresh, and signed bridge advisories can enqueue airport alerts directly. Outbox commits wake the dispatcher immediately, while a 200 ms fallback recovers ready retries. Provider acceptance, device arrival, end-to-end alert latency and behavior on specific onboard networks require separate measurement.
- An optional AeroAPI reconciler registers departure-local route and day scopes for tracked flights. It stores an ownership marker before attempting creation, recovers ambiguous results by exact marker and scope, and verifies ownership before deleting a registration. Callback refreshes require a known, tracked provider instance ID. [Aviation callbacks](docs/AVIATION_CALLBACKS.md)
- The Android app widget shows a single flight’s phase, progress, and countdown. Compatible Android hosts can place it on the Lock Screen. A device-local opt-in adds the applicable gate, terminal, and seat from accepted live pushes. Turning that setting off clears those details from the widget cache. [Android lock-screen widget support](https://android-developers.googleblog.com/2025/03/widgets-on-lock-screen-faq.html)
- A separate radar worker samples an operator receiver and stores bounded-retention, ordered positions for tracked airborne flights. Installation checks gate local receiver data. Provider and receiver history share the flight view, with cursor-based updates while the flight is active.
- One-use Friends invitations create directed relationships. Each flight grant follows the sender’s tracking state. Public pickup links expire and have independently revocable handles. [Flight sharing](docs/FLIGHT_SHARING.md)
- Optional TripIt subscriptions store encrypted feed URLs scoped to an installation. A leased worker matches dated calendar entries and commits imports only if the subscription generation still matches. The optional reservation bridge receives lookup credentials on explicit request and returns dated flight designators for user review. [Flight imports](docs/FLIGHT_IMPORTS.md)
- Every product route is available without a billing or entitlement check. External feed access, Apple signing and cloud push credentials belong to the instance operator. The self-host service can also run against explicitly labeled synthetic data for local evaluation. [Self-hosting](docs/SELF_HOSTING.md)

## Verification still required for full parity

| Area | Required evidence |
| --- | --- |
| Flighty internals | Lawfully available native package or runtime capture, with REA target identity and artifact or trace evidence for each claimed subsystem. |
| Push and native activities | Signed iOS and Android builds, both live provider credentials, platform-version and lifecycle tests, device receipt timestamps, and tests on constrained onboard Wi-Fi. |
| Aviation ingestion | Approved live source accounts and observed coverage for AeroAPI callbacks, terminal, baggage, boarding, historical tracks, raw SWIM and EUROCONTROL data and aircraft registry enrichment. |
| Prediction | Versioned training observations, holdout/backtest metrics, and validation of turnaround, weather, ATC and NOTAM attribution. A live fitted artifact and production holdout are still required. |
| Connections and sync | Verified airport route data and gate accuracy, calendar scheduling on devices, a live inbound mail transport and TripIt feed, and authorized airline-side PNR lookup. |
| Passport and Friends | Live coordinate coverage and signed-device verification of Passport, sharing, and the Friends workflow. |
| Self-host deployment | Phone-to-server TLS, live credential mounts, restore testing and operation under expected load. |

## Verified self-host paths

The Go test suite exercises installation isolation, outbox persistence and retries, provider wire contracts, and the aviation fixture adapters. A standalone localhost smoke test verified the health endpoint, flight lookup, tracking and database persistence after restart. A rootless Podman smoke verified the image as UID 10001 with a read-only root and mounted SQLite storage. Two successive containers served the same installation token and tracked flight. Compose configuration also validated with the explicit health check and optional credential mounts.
