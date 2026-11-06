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

## Serve HTTPS with Compose

Use the bundled Caddy overlay on a host where Docker Compose 2.24.4 or later
can publish TCP ports 80 and 443. Point your domain's DNS records at the host
and allow those ports through the firewall. Set `PLANO_DOMAIN` in `.env` to
the bare hostname, such as `plano.example.net`. The overlay uses that name
for both TLS and Plano's public share and callback URLs. Only Caddy publishes
ports. [Caddy automatic HTTPS](https://caddyserver.com/docs/automatic-https)

Set `PLANO_DEMO=false` for live data or `PLANO_DEMO=true` for a test with
synthetic flights. Both modes require a private enrollment code in this
overlay. For live data, configure your providers as described below. If you
use `compose.credentials.yaml` to mount secret files, add
`-f compose.credentials.yaml` after the HTTPS overlay on each command.

```sh
docker compose -f compose.yaml -f compose.https.yaml run --build --rm --no-deps plano check-config
docker compose -f compose.yaml -f compose.https.yaml up --build --wait
curl --fail https://plano.example.net/healthz
```

Keep the `caddy_data` volume across upgrades. It holds certificates and
private keys. Run the HTTPS health check from a network your phones will use
to verify DNS, certificate issuance, and routing.

## Connect the mobile clients

Physical phones need a reachable HTTPS origin such as `https://plano.example.net`.
If you already run a TLS reverse proxy, use the base Compose file, keep its
published port on loopback, and forward that origin to `http://127.0.0.1:8080`.
If your proxy runs in a container, attach it to Plano's network and forward
to `http://plano:8080`. Set `PLANO_PUBLIC_URL` to the same HTTPS origin so
share links open on other devices.

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

Upgrade the server before rolling out the current iOS app. The app includes an
opaque session binding when registering ActivityKit tokens. An earlier server
can reject those registrations. Existing unbound activities end when the
updated app next configures Live Activities for that installation.

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

The preview APK uses the Android debug key. Set `PLANO_ANDROID_PACKAGE` to
a distinct package such as `app.plano.preview` before preview prebuild if you
want the preview and a separately signed release installed side by side.
For a release signed with your
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
| Signed flight snapshots | Set `PLANO_FLIGHT_OBSERVATION_KEY_FILE` to a mounted key file if your authorized bridge supplies complete flight snapshots. [Ingress and relay setup](AVIATION_CALLBACKS.md#signed-flight-snapshots) |
| FAA SWIM radar | Set `PLANO_SWIM_POSITION_KEY_FILE` to a dedicated mounted key file if your authorized bridge supplies permitted airborne positions. [Radar relay setup](AVIATION_CALLBACKS.md#faa-swim-radar-positions) |
| EUROCONTROL radar | Set `PLANO_EUROCONTROL_POSITION_KEY_FILE` to a separate mounted key file if your authorized bridge supplies observed airborne positions permitted for enrolled trackers. [Radar relay setup](AVIATION_CALLBACKS.md#eurocontrol-radar-positions) |
| FAA NAS Status | Public aviation advisories are enabled in live mode by default. Set `PLANO_FAA_NAS=false` to disable this feed. |
| NOAA Aviation Weather Center | Set `PLANO_METAR=true` for recent airport observations or `PLANO_TAF=true` for terminal forecasts in the delay outlook. Either can run independently in live mode without an account key. |
| FAA SWIM | Set `PLANO_SWIM_URL` to your advisory bridge. If it requires authentication, set either `PLANO_SWIM_TOKEN_FILE` or `PLANO_SWIM_TOKEN`. |
| EUROCONTROL Network Manager | Set `PLANO_EUROCONTROL_URL` to your advisory bridge. If it requires authentication, set either `PLANO_EUROCONTROL_TOKEN_FILE` or `PLANO_EUROCONTROL_TOKEN`. |
| readsb ADS-B | Set `PLANO_ADSB_URL` to an operator receiver's `aircraft.json` endpoint. Coverage depends on the receiver. |
| FAA aircraft registry | Set `PLANO_FAA_REGISTRY_FILE` to the mounted FAA release ZIP for US aircraft identity, model, and approximate age. An extracted `MASTER.txt` also supplies identity and age. |
| Transport Canada aircraft registry | Set `PLANO_CCAR_REGISTRY_FILE` to the mounted `ccarcsdb.zip` for Canadian aircraft model, approximate age, and receiver matching. [Download and licence](https://wwwapps.tc.gc.ca/Saf-Sec-Sur/2/CCARCS-RIACC/DDZip.aspx) |
| Australian aircraft registry | Set `PLANO_CASA_REGISTRY_FILE` to the mounted `acrftreg.csv` for Australian aircraft model and approximate age. [CASA register download](https://www.casa.gov.au/aircraft/aircraft-registration/data-files-registered-aircraft) |
| Apple Push Notification service | Set `PLANO_APNS_TEAM_ID`, `PLANO_APNS_KEY_ID`, `PLANO_APNS_BUNDLE_ID`, and `PLANO_APNS_PRIVATE_KEY_FILE`. Set `PLANO_APNS_SANDBOX=true` for development tokens issued by the sandbox. |
| Firebase Cloud Messaging | Set `PLANO_FCM_SERVICE_ACCOUNT_FILE` for the project registered by the Android build. `PLANO_FCM_PROJECT_ID` can override the project ID in that file. |

`PLANO_REFRESH_INTERVAL` defaults to `60s` and accepts durations from `5s` to
`30m`. Periodic polling refreshes up to two tracked flights concurrently.
Shorter polling intervals can consume provider quotas faster. Set
`PLANO_FCM_BANDWIDTH_CONSTRAINED=true` only after validating delivery with the
Android build on a supported constrained network.

Each optional weather worker requests up to 24 ICAO station codes at a time,
rotating across airports with pending tracked arrivals or departures. METAR
refreshes every five minutes and expires 90 minutes after observation. TAF
refreshes every 15 minutes for operations within the next 24 hours. The delay
outlook shows adverse forecast periods that cover the expected departure or
arrival, with the forecast window and any published probability. Weather is
shown as airport context alongside the operational reasons and delay estimate.
[Aviation Weather Center data API](https://aviationweather.gov/data/api/) ·
[Terminal forecast change groups](https://aviationweather.gov/help/data/)

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

An authorized bridge can also forward flight-specific FAA EDCT and EUROCONTROL
CTOT assignments with `plano forward-atfm-slot`. It uses the same advisory
key and requires a verified flight instance match. The relay accepts normalized
JSON, supported FAA TFMData FlightControl XML, and EUROCONTROL ADEXP slot
messages. [Takeoff slot relay](AVIATION_CALLBACKS.md#flight-specific-takeoff-slots)

An authorized FAA SWIM bridge can also send normalized airborne positions
with `plano forward-swim-position`. Its `sfdps-track` mode accepts supported
FIXM-NAS 3.0 surveillance reports with an operator-supplied flight binding.
The relay uses its own signing key and delivers fixes to private radar for
verified tracked legs. [Radar relay](AVIATION_CALLBACKS.md#faa-swim-radar-positions)

An authorized EUROCONTROL bridge can relay observed airborne fixes with
`plano forward-eurocontrol-position`. Its `cat062-track` mode accepts CAT062
edition 1.21 binary blocks with an operator-supplied flight binding. Mount its
dedicated signing key on the relay and server.
[Radar relay](AVIATION_CALLBACKS.md#eurocontrol-radar-positions) ·
[CAT062 input](AVIATION_CALLBACKS.md#asterix-cat062-system-track-reports)

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
from the service's runtime. `plano check-config` validates the override's URL
without contacting it. Use an HTTP or HTTPS endpoint without URL credentials.
The probe bypasses HTTP proxies and rejects redirects.

For an iOS push-capable build, set `PLANO_IOS_TEAM_ID` to the Apple signing
team ID. Set `PLANO_IOS_BUNDLE_ID` for a custom bundle identifier and use the
same identifier for `PLANO_APNS_BUNDLE_ID` on the server. The app and activity
extension require signing and their matching provisioning profiles. Before
iOS prebuild, set
`PLANO_IOS_APNS_ENVIRONMENT=development` for a development build and set
`PLANO_APNS_SANDBOX=true` on its server. For distribution, set
`PLANO_IOS_APNS_ENVIRONMENT=production` and leave `PLANO_APNS_SANDBOX` unset
or false. The app build defaults to `development`. Rerun iOS prebuild if
you change the environment. The provisioning profile
determines the signed app's entitlement. Check `aps-environment` on the
exported app before enabling push. [Apple APNs environment](https://developer.apple.com/documentation/bundleresources/entitlements/aps-environment)
For iOS foreground refresh from alerts, use the same HTTPS origin in
`PLANO_PUBLIC_URL` and in the app's server settings.

For an Android push-capable build, set `PLANO_GOOGLE_SERVICES_FILE` to the
Firebase `google-services.json` path. The Firebase Android application must
match the build's package identifier, which `PLANO_ANDROID_PACKAGE` can set.
Use the same Firebase project for the server's FCM credentials. Test tokens
from the exact build and environment you deploy. To switch to a build without
Firebase, generate Android in a fresh checkout. Prebuild rejects an existing
native project that still contains Firebase configuration when the file
setting is omitted.

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
snapshot per flight in each of two lead windows. The standard window covers
the two hours through scheduled gate-out. The early window covers leads over
two hours and up to 25 hours. Plano assigns each verified flight a stable lead
target of 5–120 minutes in the standard window and 125–1,500 minutes in the
early window. It saves the first usable snapshot at or below the target within
that window. This spreads routinely polled observations across lead times.
Confirm that your source license permits model training. AeroAPI capture starts
when a provider flight ID is verified. A schedule alias and a direct search
result for the same instance share the target and captured forecast. A later
cancellation or invalid gate-out correction removes the outcome from exports.
A valid correction keeps the original forecast features and schedule. Exports hash
the flight ID and omit legacy schedule aliases whose capture identity cannot
be verified. Export the standard cohort from the running Compose project.

Enable `PLANO_TAF=true` while collecting data to evaluate terminal weather.
Plano records whether a prevailing adverse forecast was already available in
its cache for the expected departure or arrival. Temporary conditions,
published probabilities, and weather transitions remain visible as context.

```sh
umask 077
docker compose exec -T plano plano export-delay-samples > examples.jsonl
docker compose exec -T plano plano-train -input - -output - \
  < examples.jsonl > delay-model.json
```

The trainer reports holdout metrics on stderr. The default gate requires eight
or more holdout rows with an operational or advisory signal and learned coverage
of at least 50% within that group. Mean absolute error across the full holdout
must beat both baselines. Mount a validated artifact read-only and
set `PLANO_DELAY_MODEL_FILE` to its container path. [Delay model training](../cmd/plano-train/README.md)

A weather-aware artifact also has to beat a fit trained on the same examples
without the terminal weather term. That comparison covers both the full
holdout and the weather-bearing flights. Keep `PLANO_TAF=true` when running
either a standard or early weather-aware artifact.

Export and fit the early cohort separately.

```sh
docker compose exec -T plano plano export-delay-samples --early > early-examples.jsonl
docker compose exec -T plano plano-train -input - -output - \
  < early-examples.jsonl > early-delay-model.json
```

Mount the validated early artifact read-only and set
`PLANO_EARLY_DELAY_MODEL_FILE` to its container path. Plano uses it for forecasts
made more than two hours before scheduled departure and uses the standard model
at shorter lead times. The estimator falls back to its operational calculation
when the selected model does not support the observed inputs.

To measure a deployed model on separately collected later outcomes, use the
path mounted inside the container and a later sample export from the same lead
window.

```sh
docker compose exec -T plano plano-train \
  -evaluate-model /path/to/mounted-delay-model.json -input - < later-examples.jsonl
```

Live Plano includes a [BTS departure-risk model](../internal/historicalrisk/bts-departure-risk-2026-06.json).
To replace it with a later fit, mount the new file read-only and set
`PLANO_HISTORICAL_RISK_FILE` to its container path. For example, set the
variable to `/models/departure-risk.json` and add this mount to the `plano`
service in your Compose override.

```yaml
services:
  plano:
    volumes:
      - ./historical-departure-risk.json:/models/departure-risk.json:ro
```

Plano displays the historical probability in **Delay outlook** for supported
upcoming U.S. domestic flights. It appears separately from live forecasts and
expires one year after the training data becomes available. [Train and evaluate a BTS model](../cmd/plano-train/README.md#fit-a-historical-departure-risk-model-from-bts)

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
If a delayed remote start arrives after you untrack a flight, the app can
dismiss that activity when the server confirms the flight is missing from
your tracked list.

Turning off alerts or switching servers records a cutoff for the previous
installation. Plano retries a pending revocation when the app returns to the
foreground. Once the server receives it, earlier iOS registrations are retired
and late requests at or below the cutoff are ignored. Registrations made after
re-enabling alerts use newer revisions.

An untrack request clears local live progress only for the alert session
that initiated it. If you switch servers or restart alerts before the
request returns, the new session keeps its own progress.

Plano requests APNs priority 10 for Live Activity phase, gate, and terminal
changes. ETA-only updates and routine refreshes use priority 5. A routine
refresh leaves an outstanding important update and its retry schedule in
place. APNs and the device control actual delivery.
[Apple ActivityKit update frequency](https://developer.apple.com/documentation/ActivityKit/starting-and-updating-live-activities-with-activitykit-push-notifications#Determine-the-update-frequency)

The provider may keep reporting the planned destination after a diversion.
During a diversion, both apps show the diversion status and hide arrival
timing, progress, gate, and terminal details.

The departure window controls new remote live progress start requests. Existing
surfaces keep receiving updates if a delay moves the flight outside that
window or the departure time becomes unknown.

Before submitting a queued start, Plano checks the latest tracked flight and
drops ineligible requests. An eligible, unsent iOS start picks up the latest
flight state and seat. Android keeps the original start observation so the
device can reject it after a local stop. For iOS, Plano records each possible
submission before contacting APNs. A lost response or interrupted attempt
keeps the record with the tracked flight and notification session, including
after the start token changes. A definite rejection permits a retry. If a
start never appears after an interrupted attempt, open the tracked flight
and start its Live Activity from the app.

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
and their delivery window. Android uses the original observation time to
order delayed live updates, including updates from the same second. Older
queued messages still use their whole-second time and FCM send time. When a
precise observation and an older-format observation share a second, Android
prefers the precise one. A received end update also prevents the widget
from showing cached progress for that flight after an app restart.
[FCM message lifespan](https://firebase.google.com/docs/cloud-messaging/customize-messages/setting-message-lifespan) · [FCM Android priorities](https://firebase.google.com/docs/cloud-messaging/android-message-priority)

When Android hides private notification content, Plano supplies a lock-screen
version with phase, progress, and timing. Flight number, route, gate,
terminal, and seat stay in the private notification. The phone's settings
control lock-screen display and Live Update promotion. On Android 8 and later,
a progress notification can show a live countdown and requests expiry at its
target or 20 minutes after the flight observation, whichever comes first. A
fresh push can restore progress after the countdown ends. Android 7 uses a
static system timestamp and an inexact alarm for the observation deadline.
Swiping away a progress notification keeps it dismissed for that live
session. The flight widget can still receive updates.
Choose **Stop live progress** on the private notification to clear that
flight's progress from the notification and widget. Flight tracking and
ordinary alerts continue. The device ignores queued progress until a newer
live start. Removing and tracking the flight again allows a fresh start.
[Android notification timeout](https://developer.android.com/reference/android/app/Notification.Builder#setTimeoutAfter(long)) · [Android Live Update dismissal](https://developer.android.com/develop/ui/views/notifications/live-update#dismissal)

The Android widget shows phase, progress, and a labeled departure or arrival
estimate in the device's local time. With Android flight alerts enabled,
open a tracked flight and choose **Show on flight widget** to pin it on this
device. The widget waits for a fresh update for the pinned flight. Choose
**Use automatic widget selection** on that flight to resume automatic selection.
Receiving the flight's end update, stopping its progress, or ending the
current alert session clears the pin. If the end update is missed, automatic
selection is still available from the finished flight's details.

In Plano Settings, choose **Show flight details on widget** to include the applicable
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

## Transport Canada aircraft registry

Download `ccarcsdb.zip` from Transport Canada under the terms on its download
page. Mount it read-only at a path such as `/data-import/ccarcsdb.zip` and set
`PLANO_CCAR_REGISTRY_FILE` to that path. Plano reads the archive during
`check-config` and at live startup. It can use the Canadian and FAA registries
together. Replace the file with a current release and restart to refresh it.
[Transport Canada download and licence](https://wwwapps.tc.gc.ca/Saf-Sec-Sur/2/CCARCS-RIACC/DDZip.aspx)

Plano uses current registration data to add a model designation, an approximate
age based on the manufacture or assembly year, and a recorded Mode S address for a local
receiver. It reads the aircraft table and its field layout. The owner table,
aircraft serial numbers, and manufacturer name fields stay out of the index.
When using the dataset to serve other people, apply written terms consistent
with the licence's distribution requirements in section 3.1.

> Reproduced and distributed with the permission of the Government of Canada.
>
> This product has been produced by or for Plano and includes data provided by
> the Government of Canada.
>
> The incorporation of data sourced from the Government of Canada within this
> product shall not be construed as constituting an endorsement by the
> Government of Canada of our product.
>
> CANADA MAKES NO REPRESENTATION AND GIVES NO WARRANTY OF ANY KIND WITH RESPECT
> TO THE ACCURACY, USEFULNESS, NOVELTY, VALIDITY, SCOPE, COMPLETENESS OR CURRENCY
> OF THE DATA MADE AVAILABLE THROUGH THE GC OPEN DATA PORTAL AND EXPRESSLY
> DISCLAIMS ANY IMPLIED WARRANTY OF MERCHANTABILITY OR FITNESS FOR A PARTICULAR
> PURPOSE OF THE DATA MADE AVAILABLE THROUGH THE GC OPEN DATA PORTAL.

## Australian aircraft registry

Download CASA's current `acrftreg.csv`, mount it read-only at a path such as
`/data-import/acrftreg.csv`, and set `PLANO_CASA_REGISTRY_FILE` to that path.
If you download the ZIP, extract the CSV before mounting it. Plano validates
the file during `check-config` and loads it at live startup. Replace it with
a current release and restart to refresh it. The FAA, Transport Canada, and
CASA registers can run together. [CASA register download](https://www.casa.gov.au/aircraft/aircraft-registration/data-files-registered-aircraft)

Plano uses the current VH registration, model, and year of manufacture for
assigned aircraft on near-term flights. The date the aircraft was first
registered in Australia is a lower bound on that lookup. Plano derives an
approximate age for display and keeps the source attribution with the fields.
The local index excludes owner, operator, and serial-number data. [CASA field definitions](https://www.casa.gov.au/aircraft/aircraft-registration/data-files-registered-aircraft/meaning-coded-fields-registered-aircraft) · [CASA copyright and licence](https://www.casa.gov.au/about-us/about-website/copyright)

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
server reports the reset. When you open the immediate inbound leg from
**Where’s my plane?**, its radar view includes receiver fixes while Plano can
verify the aircraft and your onward flight is still tracked. Receiver fixes
remain available during a provider track outage while the inbound observation
is fresh.

When opening a tracked flight or refreshing a replaced track, the app requests
a bounded sample across the retained radar history. The map and charts show
up to 512 fixes across that time range, including the first and latest fixes.
The map uses bundled Natural Earth coastlines. The latest position identifies
its source and UTC observation time. Altitude, ground speed and vertical motion
appear when the source reports them. The map and charts break the track when
displayed fixes are more than five minutes apart. Isolated fixes remain visible.
Altitude and speed graphs use elapsed time between reports. Tracking an
airborne demo flight shows simulated positions after the next refresh.

Friends and pickup links use separate permissions on the same instance.
[Flight sharing](FLIGHT_SHARING.md)
