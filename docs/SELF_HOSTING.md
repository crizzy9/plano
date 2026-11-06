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

The app's search accepts demo flight `UA123` for the current date. You can
also check the demo from a terminal with `jq` installed.

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

For a live server reachable from the Internet, require an enrollment code
before connecting devices. Generate a random code of 16 to 128 bytes and
provide it through `PLANO_ENROLLMENT_CODE_FILE` or `PLANO_ENROLLMENT_CODE`.
Share the code privately with new users so they can connect to the server.
Existing installations retain access when you change the enrollment code.

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
| FAA SWIM | Set `PLANO_SWIM_URL` to your advisory bridge. If it requires authentication, set either `PLANO_SWIM_TOKEN_FILE` or `PLANO_SWIM_TOKEN`. |
| EUROCONTROL Network Manager | Set `PLANO_EUROCONTROL_URL` to your advisory bridge. If it requires authentication, set either `PLANO_EUROCONTROL_TOKEN_FILE` or `PLANO_EUROCONTROL_TOKEN`. |
| readsb ADS-B | Set `PLANO_ADSB_URL` to an operator receiver's `aircraft.json` endpoint. Coverage depends on the receiver. |
| FAA aircraft registry | Set `PLANO_FAA_REGISTRY_FILE` to a mounted, unzipped FAA `MASTER.txt` file to enrich US aircraft identities and approximate ages. |
| Apple Push Notification service | Set `PLANO_APNS_TEAM_ID`, `PLANO_APNS_KEY_ID`, `PLANO_APNS_BUNDLE_ID`, and `PLANO_APNS_PRIVATE_KEY_FILE`. Set `PLANO_APNS_SANDBOX=true` for development tokens issued by the sandbox. |
| Firebase Cloud Messaging | Set `PLANO_FCM_PROJECT_ID` and `PLANO_FCM_SERVICE_ACCOUNT_FILE` for the project registered by the Android build. |

`PLANO_REFRESH_INTERVAL` defaults to `60s` and accepts durations from `5s` to
`30m`. Shorter polling intervals can consume provider quotas faster. Set
`PLANO_FCM_BANDWIDTH_CONSTRAINED=true` only after validating delivery with the
Android build on a supported constrained network.

The FAA SWIM and EUROCONTROL options consume an operator-managed bridge with
authorized upstream access. Each bridge serves an HTTPS endpoint whose `GET`
response is a JSON snapshot. A bridge with no active advisories returns
`{"version":1,"advisories":[]}`. See the parser and struct definitions for
advisory validation rules and fields. [Gateway parser](../internal/feeds/gateway.go),
[Advisory fields](../internal/feeds/feeds.go)
Use HTTPS for bridges outside the Plano container. HTTP is accepted only at
an IP loopback address, which resolves inside the container in Compose.
The same endpoint rules apply to readsb. [Feed adapter guide](../internal/feeds/README.md)

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
their own target paths. Mount the FAA registry file read-only at a path such
as `/data-import/MASTER.txt` and set `PLANO_FAA_REGISTRY_FILE` to that path.

```sh
docker compose -f compose.yaml -f compose.credentials.yaml up --build --wait
```

Push-capable builds also need Apple signing and push entitlements on iOS or
matching Firebase app configuration on Android. Test tokens from the exact
build and environment you deploy.

## Maintain and back up

The named volume `plano_data` holds SQLite and its WAL files. Normal service
recreation preserves the volume. To make a consistent cold backup, stop
Plano and copy the entire data directory into a private backup directory.

```sh
umask 077
backup=$(mktemp -d ./plano-backup.XXXXXX)
docker compose stop plano
docker compose cp plano:/data/. "$backup/"
docker compose start plano
```

Use the same Compose files for maintenance commands if you start with an
override. Keep the backup private because the database includes device tokens
and flight-tracking state. Restore the complete backup to a dedicated data
directory while Plano is stopped, mount that directory at `/data`, and
verify the container user can write to it before starting the service.

## Run without containers

Use Go 1.26 and a C compiler for the SQLite driver.

```sh
umask 077
CGO_ENABLED=1 GOMAXPROCS=2 go build -p 1 -o plano ./cmd/plano
mkdir -p ./data
PLANO_ADDR=127.0.0.1:8080 PLANO_DB=./data/plano.db \
  PLANO_PUBLIC_URL=http://localhost:8080 PLANO_DEMO=true ./plano
```
