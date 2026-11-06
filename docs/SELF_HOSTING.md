# Self-host Plano

Plano runs as one Go service that owns the API, aviation refresh worker, push
outbox, and SQLite database. Deploy one service instance per local SQLite
volume. The iOS and Android clients can use your deployment as their API.

## Start a local demo

Run these commands from the Plano repository root with Docker and the Compose
plugin installed.

```sh
docker compose up --build --wait
curl --fail http://localhost:8080/healthz
```

The default binds to loopback and runs on port 8080. Demo mode uses synthetic
flight data and needs no provider accounts. The health endpoint reports
`{"status":"ok","mode":"demo"}`.

The app's search accepts demo flights `UA123` and `UA456`. Track both for
tomorrow's date to try a suggested connection through Newark. You can also
check the demo from a terminal with `jq` installed.

```sh
token=$(curl --fail --silent --show-error http://localhost:8080/v1/installations \
  -H 'Content-Type: application/json' -d '{}' | jq -er '.token')
curl --fail --silent --show-error --get http://localhost:8080/v1/flights \
  -H "Authorization: Bearer $token" \
  --data-urlencode 'number=UA123' --data-urlencode "date=$(date -u +%F)"
```

Set local overrides in `.env` using `.env.example` as a reference. If you change
`PLANO_PORT`, set `PLANO_PUBLIC_URL` to the corresponding public origin.

## Connect the mobile clients

For physical phones, configure a reachable HTTPS origin such as
`https://plano.example.net`. Keep the Compose port on loopback and configure
your TLS reverse proxy to forward that origin to `http://127.0.0.1:8080`. If
the proxy runs inside another container, attach it to the service's network
and forward to `http://plano:8080`. Set `PLANO_PUBLIC_URL` to the same
HTTPS origin so share links open on other devices.

For example, a host running Caddy can use the following site block. Point
the domain's DNS records at the server and expose ports 80 and 443 to Caddy.
[Caddy reverse proxy guide](https://caddyserver.com/docs/quick-starts/reverse-proxy)

```caddyfile
plano.example.net {
    reverse_proxy 127.0.0.1:8080
}
```

In the app, open **Settings** → **Plano server**, enter your HTTPS origin
under **Server address**, and tap **Connect to server**. Plano checks the
health endpoint before saving the address. For a custom app build, you can
set the default origin using Node.js 22.13 or later and the native SDK.

```sh
cd mobile
npm ci
EXPO_PUBLIC_API_URL=https://plano.example.net npm run android
# On macOS with Xcode
EXPO_PUBLIC_API_URL=https://plano.example.net npm run ios
```

`EXPO_PUBLIC_API_URL` is a build-time default. Settings lets each installation
choose a different server at runtime. Keep provider keys on the server.

The pinned Expo version uses Android SDK 36, Build-Tools 36, and NDK
`27.1.12297006`. Expo Notifications builds from source on Android so Plano's
native messaging service can extend it.

For an installable Android preview that runs without Metro, generate the
Android project and assemble the release variant with preview signing.

```sh
cd mobile
npm ci
npx expo prebuild --platform android --no-install
cd android
PLANO_ANDROID_PREVIEW_BUILD=1 ./gradlew :app:assembleRelease
```

The preview APK uses the Android debug key. For a release signed with your
own key, omit `PLANO_ANDROID_PREVIEW_BUILD` and set all four signing inputs.

| Variable | Value |
| --- | --- |
| `PLANO_ANDROID_KEYSTORE_FILE` | Absolute path to the keystore |
| `PLANO_ANDROID_KEY_ALIAS` | Alias of the signing key |
| `PLANO_ANDROID_STORE_PASSWORD_FILE` | Absolute path to the keystore password file |
| `PLANO_ANDROID_KEY_PASSWORD_FILE` | Absolute path to the signing key password file |

Gradle reads the passwords at build time. A release with no signing inputs
is unsigned. Keep the keystore and password files outside the source tree.

Live mode requires an enrollment code by default. Generate a random code of 16 to 128 bytes and
provide it through `PLANO_ENROLLMENT_CODE_FILE` or `PLANO_ENROLLMENT_CODE`.
Share the code privately with new users so they can connect to the server.
Existing installations retain access when you change the enrollment code.
On a private live deployment where open enrollment is intended, set
`PLANO_ALLOW_OPEN_ENROLLMENT=true` and restrict network access to that deployment.
Set one of these options before upgrading an existing live deployment that
used open enrollment.

## Enable live data and push

Set `PLANO_DEMO=false` and provide an AeroAPI key with permission for the
endpoints you plan to use. Plano's source is self-hosted. Live airline data
and OS push delivery use the configured external providers and are subject
to those providers' access and quotas.

Use a separate Compose project for live data if you want to keep a demo's
database. Each project creates its own `plano_data` volume. Use a different
published port if both projects run on the same host.

| Provider | Configuration |
| --- | --- |
| FlightAware AeroAPI | Set `PLANO_AEROAPI_KEY_FILE` to a mounted key file, or supply `PLANO_AEROAPI_KEY` through the service environment. |
| FAA NAS Status | Public aviation advisories are enabled in live mode by default. Set `PLANO_FAA_NAS=false` to disable this feed. |
| NOAA Aviation Weather Center | Set `PLANO_METAR=true` in live mode to show recent adverse airport observations in the delay outlook. No account key is needed. |
| FAA SWIM | Set `PLANO_SWIM_URL` to your advisory bridge. If it requires authentication, set either `PLANO_SWIM_TOKEN_FILE` or `PLANO_SWIM_TOKEN`. |
| EUROCONTROL Network Manager | Set `PLANO_EUROCONTROL_URL` to your advisory bridge. If it requires authentication, set either `PLANO_EUROCONTROL_TOKEN_FILE` or `PLANO_EUROCONTROL_TOKEN`. |
| readsb ADS-B | Set `PLANO_ADSB_URL` to an operator receiver's `aircraft.json` endpoint. Coverage depends on the receiver. |
| FAA aircraft registry | Set `PLANO_FAA_REGISTRY_FILE` to the mounted FAA release ZIP for US aircraft identity, model, and approximate age. An extracted `MASTER.txt` also supplies identity and age. |
| Apple Push Notification service | Set `PLANO_APNS_TEAM_ID`, `PLANO_APNS_KEY_ID`, `PLANO_APNS_BUNDLE_ID`, and `PLANO_APNS_PRIVATE_KEY_FILE`. Set `PLANO_APNS_SANDBOX=true` for development tokens issued by the sandbox. |
| Firebase Cloud Messaging | Set `PLANO_FCM_SERVICE_ACCOUNT_FILE` for the project registered by the Android build. `PLANO_FCM_PROJECT_ID` can override the project ID in that file. |

`PLANO_REFRESH_INTERVAL` defaults to `60s` and accepts durations from `5s` to
`30m`. Shorter polling intervals can consume provider quotas faster. Set
`PLANO_FCM_BANDWIDTH_CONSTRAINED=true` only after validating delivery with the
Android build on a supported constrained network.

The optional weather worker requests at most 24 ICAO station codes every five
minutes, rotating across airports with imminent tracked arrivals or departures.
Requests contain station codes. The delay view for a tracked flight shows report time and observed
conditions separately from operational reasons and model estimates. Reports
expire 90 minutes after observation. [Aviation Weather Center data API](https://aviationweather.gov/data/api/)

The FAA SWIM and EUROCONTROL options consume an operator-managed bridge with
authorized upstream access. Each bridge serves an HTTPS endpoint whose `GET`
response is a JSON snapshot. A bridge with no active advisories returns
`{"version":1,"advisories":[]}`. See the parser and struct definitions for
advisory validation rules and fields. [Gateway parser](../internal/feeds/gateway.go),
[Advisory fields](../internal/feeds/feeds.go)
Use HTTPS for bridges outside the Plano container. HTTP is accepted only at
an IP loopback address, which resolves inside the container in Compose.
The same endpoint rules apply to readsb. [Feed adapter guide](../internal/feeds/README.md)

For signed delivery, run `plano forward-advisory` alongside your authorized
subscriber. It accepts normalized JSON or supported AIXM Digital NOTAM XML.
[Advisory relay setup](AVIATION_CALLBACKS.md#faa-swim-and-eurocontrol-bridges)

Keep credential files outside the checkout. Compose reads the `*_FILE`
values as paths **inside** the container. For each credential with both
inputs, choose either the direct value or the file input. An optional
override such as `compose.credentials.yaml` can mount the files you use.

```yaml
services:
  plano:
    volumes:
      - type: bind
        source: /path/to/private/aeroapi-key
        target: /run/secrets/aeroapi-key
        read_only: true
        bind:
          create_host_path: false
      - type: bind
        source: /path/to/private/enrollment-code
        target: /run/secrets/enrollment-code
        read_only: true
        bind:
          create_host_path: false
      - type: bind
        source: /path/to/private/apns-key.p8
        target: /run/secrets/apns-key.p8
        read_only: true
        bind:
          create_host_path: false
      - type: bind
        source: /path/to/private/firebase-service-account.json
        target: /run/secrets/firebase-service-account.json
        read_only: true
        bind:
          create_host_path: false
```

Include only mounts for the providers you enable. Set each corresponding
`*_FILE` variable to its `target` value. Grant the container's UID 10001 or
group 10001 read access through the host's container ID mapping. Bind mounts
preserve host permissions. Bridge tokens can use the same mount pattern at
their own target paths. Mount the [FAA registry archive](https://www.faa.gov/licenses_certificates/aircraft_certification/aircraft_registry/releasable_aircraft_download/index.cfm)
read-only at a path such as `/data-import/ReleasableAircraft.zip` and set
`PLANO_FAA_REGISTRY_FILE` to that path. Plano reads the archive at startup.
Use a current release and restart the service after replacing it.

```sh
docker compose -f compose.yaml -f compose.credentials.yaml run --build --rm --no-deps plano check-config
docker compose -f compose.yaml -f compose.credentials.yaml up --build --wait
```

`plano check-config` validates startup settings and reads configured credential
files without opening the database, binding the port, or contacting providers.
It exits nonzero for partial push configuration. After startup, `plano healthcheck`
checks the service and its database through `/healthz`. It uses `PLANO_ADDR` by
default. Set `PLANO_HEALTH_URL` for a different direct probe endpoint reachable
from the service's runtime. The probe bypasses HTTP proxies and rejects redirects.

For an iOS push-capable build, set `PLANO_IOS_TEAM_ID` to the Apple signing
team ID. Set `PLANO_IOS_BUNDLE_ID` for a custom bundle identifier and use the
same identifier for `PLANO_APNS_BUNDLE_ID` on the server. The app and activity
extension require signing and their matching entitlements.

For an Android push-capable build, set `PLANO_GOOGLE_SERVICES_FILE` to the
Firebase `google-services.json` path. The Firebase Android application must
match the build's package identifier, which `PLANO_ANDROID_PACKAGE` can set.
Use the same Firebase project for the server's FCM credentials. Test tokens
from the exact build and environment you deploy.

## Maintain and back up

The named volume `plano_data` holds SQLite and its WAL files. Normal service
recreation preserves the volume. `plano backup` captures a consistent snapshot
while the service runs, including committed changes still in the WAL. It
checks the snapshot, then publishes one SQLite file with mode `0600` at a
new destination. The destination filesystem must support hard links.

```sh
umask 077
backup=$(mktemp -d ./plano-backup.XXXXXX)
snapshot="plano-$(date -u +%Y%m%dT%H%M%SZ)-${backup##*.}.db"
docker compose exec -T plano plano backup "/data/$snapshot" &&
  docker compose cp "plano:/data/$snapshot" "$backup/plano.db"
```

Use the same Compose files for maintenance commands if you start with an
override. After verifying the copy, remove the staged snapshot from `/data`
and retain the copy on separate storage. Leave space for the snapshot and for
WAL growth during a large backup. Keep copies private because the database
includes device tokens and flight-tracking state. Back up your Compose
configuration and mounted files separately, including the TripIt encryption
key if configured. For a standalone server, run
`PLANO_DB=./data/plano.db ./plano backup /private/backup/plano.db`.

To restore, copy `plano.db` into a fresh data directory with no prior SQLite
sidecar files. Give the container's UID and GID `10001:10001` access to write
the directory and the database, using your runtime's host UID mapping. Mount
that directory at `/data` in a Compose override and retain the original
configuration and mounted keys. Stop the previous instance before starting
the restored one with the same live configuration. A restore can retry a
pending push from the backup. Use demo mode and a separate port for a restore
drill, then check health and open a tracked flight using an existing
installation before putting the restored instance into service.

## Run without containers

Use Go 1.26 and a C compiler for the SQLite driver.

```sh
umask 077
CGO_ENABLED=1 GOMAXPROCS=2 go build -p 1 -o plano ./cmd/plano
mkdir -p ./data
PLANO_ADDR=127.0.0.1:8080 PLANO_DB=./data/plano.db \
  PLANO_PUBLIC_URL=http://localhost:8080 PLANO_DEMO=true ./plano
```

## Airport routing, imports and delay models

Set `PLANO_AIRPORT_TRANSFERS_FILE` to a read-only route graph if you maintain
terminal and gate directions. [Airport transfer configuration](AIRPORT_TRANSFERS.md)

Set `PLANO_BOOKING_KEY_FILE` to a read-only private key file to index imported
booking locators. Run `plano forward-email` as a pipe command on your inbound
mail host to deliver confirmations to the self-hosted webhook. The command
uses its own relay token file and lets the mail queue retry temporary failures.
[Flight imports](FLIGHT_IMPORTS.md)

In live mode, set `PLANO_TRIPIT_SYNC_KEY_FILE` to a mounted file with a stable
random key of at least 32 bytes to enable scheduled TripIt imports. Keep the
key with your deployment credentials when moving or restoring the database.
Set `PLANO_TRIPIT_SYNC_INTERVAL` to a duration from `5m` to `24h`, with a
default of `30m`. Removing the key pauses the worker. Installations can still
remove their subscriptions. Replacing the key requires users to re-enter
their feed URLs.

Set `PLANO_RESERVATION_GATEWAY_URL` and `PLANO_RESERVATION_GATEWAY_TOKEN_FILE`
to enable partner booking lookup in live mode. This integration uses an
operator-managed reservation bridge with licensed access. Restrict enrollment
on publicly reachable deployments and apply lookup limits at the bridge.
[Reservation gateway contract](RESERVATION_GATEWAY.md)

Set `PLANO_COLLECT_DELAY_SAMPLES=true` in live mode to collect one forecast
snapshot per flight from 30 to 120 minutes before scheduled gate-out, with
its eventual reported outcome. Confirm that your source license permits model
training. The exporter streams completed samples without passenger details or
raw provider flight IDs. From the running Compose project:

```sh
umask 077
docker compose exec -T plano plano export-delay-samples > examples.jsonl
docker compose exec -T plano plano-train -input - -output - \
  < examples.jsonl > delay-model.json
```

The trainer reports holdout metrics on stderr and writes an artifact only if
the default backtest gate passes. Mount a validated artifact read-only and
set `PLANO_DELAY_MODEL_FILE` to its container path. [Delay model training](../cmd/plano-train/README.md)

To measure a deployed model on separately collected later outcomes, use the
path mounted inside the container and a later sample export.

```sh
docker compose exec -T plano plano-train \
  -evaluate-model /path/to/mounted-delay-model.json -input - < later-examples.jsonl
```

Evaluation prints JSON metrics, excludes forecasts at or before the last
fitted label, and leaves the artifact intact.

## Optional real-time callbacks

Enable an HTTPS FlightAware callback or a signed FAA SWIM/EUROCONTROL bridge
to supplement periodic ingestion. Each callback uses its own private mounted
key file. [Aviation callbacks](AVIATION_CALLBACKS.md)

## iOS Live Activity tokens

Use the current backend and iOS build together. Plano keeps the latest
pending push-to-start and update token in memory and retries on foreground
after a temporary registration failure. ActivityKit supplies current tokens
again when the app relaunches. The server replaces an older token in the
same installation and Live Activity slot when it accepts the rotated token.
The app persists a registration counter in Keychain so the server can ignore
an older write that arrives out of order.

Turning off alerts or switching servers records a cutoff for the previous
installation. Plano retries a pending revocation when the app returns to the
foreground. Once the server receives it, earlier iOS registrations are retired
and late requests at or below the cutoff are ignored. Registrations made after
re-enabling alerts use newer revisions.

iOS state alerts for the same installation, flight, and category share an APNs
collapse ID. A newer gate or carousel revision waits for any in-flight request
in that category. Other alerts can send independently. APNs controls offline
storage and device delivery. [APNs delivery](https://developer.apple.com/documentation/usernotifications/sending-notification-requests-to-apns)

## Android delivery binding

Use the current backend and Android build together for flight alerts and the
progress widget. An Android registration carries a device-generated binding
for the selected host and alert session. Plano includes it in FCM data. The
app verifies the binding before displaying a notification or updating its
widget. Switching servers or turning alerts off invalidates the old binding
on the device, including while a remote revocation is pending.

With the current backend and Android build, delayed alerts are checked against
their expiry and recent flight revisions. A newer gate or carousel update
replaces the visible alert for that category. Movement alerts can clear older
boarding and departure alerts. Ordered alert history is scoped to the active
push session.

Unchanged live progress uses normal-priority refreshes that FCM discards if
it cannot deliver them immediately. A quiet flight's progress can expire if
the phone remains in Doze. Live starts, ends, and changes keep high priority
and their delivery window. Android checks observation order when delayed live
updates arrive.
[FCM message lifespan](https://firebase.google.com/docs/cloud-messaging/customize-messages/setting-message-lifespan) · [FCM Android priorities](https://firebase.google.com/docs/cloud-messaging/android-message-priority)

When Android hides private notification content, Plano supplies a lock-screen
version with phase, progress, and timing. Flight number, route, gate,
terminal, and seat stay in the private notification. The phone's settings
control lock-screen display and Live Update promotion. On Android 8 and later,
a progress notification can show a live countdown and requests expiry at its
target or 20 minutes after the flight observation, whichever comes first. A
fresh push can restore progress after the countdown ends. Android 7 uses a
static system timestamp and an inexact alarm for the observation deadline.
Choose **Stop live progress** on the private notification to clear that
flight's progress from the notification and widget. Flight tracking and
ordinary alerts continue. The device ignores queued progress until a newer
live start. Removing and tracking the flight again allows a fresh start.
[Android notification timeout](https://developer.android.com/reference/android/app/Notification.Builder#setTimeoutAfter(long))

The Android widget shows phase, progress, and a labeled departure or arrival
estimate in the device's local time. In Plano Settings, choose **Show flight
details on widget** to include the applicable
gate, terminal, and seat from the next flight update. This setting applies to
the device. Details can be visible on a lock screen that supports widgets.
**Hide flight details on widget** removes the details from the widget cache.
Between pushes, the widget asks Android to refresh at progress milestones
while the observation is fresh. Android can defer these refreshes while idle.

Tap a widget showing a current flight to open its details in Plano. The
widget sends an opaque key scoped to the active alert session. Plano checks
that the widget still shows the same flight and that your installation still
tracks it. Tap routing starts with the next valid flight update after an
upgrade. A stale widget tap opens Plano without selecting a flight.

## Local radar and sharing

With `PLANO_ADSB_URL` configured, the receiver worker samples matching
tracked airborne legs every five seconds by default. Set
`PLANO_ADSB_REFRESH_INTERVAL` between `3s` and `60s` to suit the receiver.
Plano skips explicit ground reports and saves a receiver fix only when it
matches one currently eligible tracked leg. Plano stops sampling stale flight
lifecycle records and retains local points for seven days. The flight screen
reads cached provider positions and receiver positions on independent cursors
while a tracked flight is airborne. Provider positions refresh on the main
`PLANO_REFRESH_INTERVAL`. The mobile app replaces its provider snapshot when
the revision changes. A withdrawn tail number or a change of aircraft or
provider instance clears the affected receiver history.
The current app resets its receiver cursor and removes those points when the
server reports the reset. Local receiver points appear only to an installation
tracking the flight.

The map uses bundled Natural Earth coastlines. The latest position identifies
its source and UTC observation time. Altitude, ground speed and vertical motion
appear when the source reports them. The map and charts break the track when
more than five minutes pass between reports. Isolated reports remain visible.
Altitude and speed graphs use elapsed time between reports. Tracking an
airborne demo flight shows simulated positions after the next refresh.

Friends and pickup links use separate permissions on the same instance.
[Flight sharing](FLIGHT_SHARING.md)
