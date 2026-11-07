#!/usr/bin/env bash
set -euo pipefail
umask 077

cd "$(dirname "$0")/.."
test -n "$RUNNER_TEMP"
test -n "$PLANO_IOS_BUNDLE_ID"
app="$RUNNER_TEMP/plano-ios/Build/Products/Release-iphonesimulator/Plano.app"
test -d "$app"
work=$(mktemp -d "$RUNNER_TEMP/plano-ios-demo.XXXXXXXX")
evidence="$RUNNER_TEMP/plano-ios-demo-evidence"
mkdir -p "$evidence"

device='' demo_pid='' proxy_pid=''
cleanup() {
  local result=$?
  if [[ -n "$device" ]]; then
    xcrun simctl io "$device" screenshot "$evidence/final.png" >/dev/null 2>&1 || true
    if (( result != 0 )); then
      xcrun simctl spawn "$device" log show --style compact --last 1m \
        --predicate 'process == "Plano"' >"$evidence/ios.log" 2>&1 || true
    fi
    xcrun simctl terminate "$device" "$PLANO_IOS_BUNDLE_ID" >/dev/null 2>&1 || true
    xcrun simctl shutdown "$device" >/dev/null 2>&1 || true
  fi
  for owned in "$proxy_pid" "$demo_pid"; do
    if [[ -n "$owned" ]]; then
      kill "$owned" 2>/dev/null || true
      wait "$owned" 2>/dev/null || true
    fi
  done
  if (( result != 0 )); then
    for name in demo proxy maestro; do
      if [[ -f "$evidence/$name.log" ]]; then tail -n 35 "$evidence/$name.log"; fi
    done
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Keep the signature and simulator capabilities available when a UI check fails.
for target in "$app" "$app/PlugIns/PlanoActivity.appex"; do
  name=${target##*/}
  executable=$(/usr/libexec/PlistBuddy -c 'Print CFBundleExecutable' "$target/Info.plist")
  codesign --display --verbose=2 "$target" >"$evidence/$name-codesign.txt" 2>&1
  if ! xcrun otool -s __TEXT __entitlements "$target/$executable" \
    >"$evidence/$name-simulator-entitlements.txt" 2>&1; then
    printf '%s\n' 'Simulator entitlement section diagnostic unavailable.' \
      >>"$evidence/$name-simulator-entitlements.txt"
  fi
done

# Keep the portable driver and its runtime state in this disposable workspace.
driver_sha=54dbb0716425626604973cbb374c585ca30272375d9ce15177a1152765ddd9d7
curl -q --fail --silent --show-error --location --retry 2 --connect-timeout 10 --max-time 180 \
  https://github.com/mobile-dev-inc/Maestro/releases/download/cli-2.12.1/maestro.zip \
  -o "$work/maestro.zip"
printf '%s  %s\n' "$driver_sha" "$work/maestro.zip" | shasum -a 256 --check
unzip -q "$work/maestro.zip" -d "$work"
export JAVA_HOME
JAVA_HOME=$(/usr/libexec/java_home -v 17)
export PATH="$JAVA_HOME/bin:$PATH"
export JAVA_TOOL_OPTIONS="-Xmx512m -Duser.home=$work/java-home"
export XDG_STATE_HOME="$work/state" XDG_CONFIG_HOME="$work/config"
export XDG_CACHE_HOME="$work/cache" XDG_DATA_HOME="$work/data"
mkdir -p "$work/java-home" "$XDG_STATE_HOME" "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_DATA_HOME"
export MAESTRO_CLI_NO_ANALYTICS=true MAESTRO_DISABLE_UPDATE_CHECK=true
export MAESTRO_CLI_ANALYSIS_NOTIFICATION_DISABLED=true MAESTRO_DRIVER_STARTUP_TIMEOUT=180000

CGO_ENABLED=1 GOMAXPROCS=2 GOMEMLIMIT=512MiB go build -p 1 -o "$work/plano" ./cmd/plano

cat >"$work/cert.cnf" <<'CONF'
[req]
prompt = no
distinguished_name = identity
[identity]
CN = 127.0.0.1
[ca]
basicConstraints = critical,CA:true
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash
[server]
basicConstraints = critical,CA:false
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = IP:127.0.0.1
CONF
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 1 \
  -subj '/CN=Plano simulator test CA' -config "$work/cert.cnf" -extensions ca \
  -keyout "$work/ca-key.pem" -out "$work/ca.pem" >"$work/cert.log" 2>&1
openssl req -new -newkey rsa:2048 -nodes -sha256 -config "$work/cert.cnf" \
  -keyout "$work/server-key.pem" -out "$work/server.csr" >>"$work/cert.log" 2>&1
openssl x509 -req -sha256 -days 1 -in "$work/server.csr" -CA "$work/ca.pem" \
  -CAkey "$work/ca-key.pem" -CAcreateserial -extfile "$work/cert.cnf" -extensions server \
  -out "$work/server.pem" >>"$work/cert.log" 2>&1

# Use the normal demo entrypoint and preserve its enrollment and bearer handling.
GOMAXPROCS=2 GOMEMLIMIT=256MiB PLANO_DEMO=true PLANO_ADDR=127.0.0.1:8080 PLANO_PUBLIC_URL=https://127.0.0.1:8443 \
  PLANO_DB="$work/plano.db" "$work/plano" >"$evidence/demo.log" 2>&1 &
demo_pid=$!
python3 mobile/scripts/ios-demo-proxy.py --cert "$work/server.pem" \
  --key "$work/server-key.pem" >"$evidence/proxy.log" 2>&1 &
proxy_pid=$!
for _readiness_attempt in {1..40}; do
  if curl -q --noproxy '*' --silent --fail --max-time 2 --cacert "$work/ca.pem" \
    https://127.0.0.1:8443/healthz | grep -q '"mode":"demo"'; then break; fi
  kill -0 "$demo_pid" "$proxy_pid"
  sleep .25
done
curl -q --noproxy '*' --fail --silent --show-error --max-time 5 --cacert "$work/ca.pem" \
  https://127.0.0.1:8443/healthz | grep -q '"mode":"demo"'

device=$(xcrun simctl list devices available --json | jq -er '
  [.devices | to_entries[]
   | select(.key | startswith("com.apple.CoreSimulator.SimRuntime.iOS-26-4"))
   | .value[]
   | select(.isAvailable and .state == "Shutdown" and (.name | startswith("iPhone")))]
  | .[0].udid | strings')
test -n "$device"
xcrun simctl boot "$device"
xcrun simctl bootstatus "$device" -b
xcrun simctl keychain "$device" add-root-cert "$work/ca.pem"
xcrun simctl install "$device" "$app"
launched=$(xcrun simctl launch --terminate-running-process "$device" "$PLANO_IOS_BUNDLE_ID")
printf '%s\n' "$launched"
pid=$(printf '%s\n' "$launched" | awk -F ': ' '/: [0-9]+$/ {print $NF}')
[[ "$pid" =~ ^[0-9]+$ ]]
sleep 10
kill -0 "$pid"
ps -p "$pid" -o command= | grep -F '/Plano.app/Plano'
xcrun simctl io "$device" screenshot "$evidence/startup.png"

python3 - "$work/maestro/bin/maestro" "$device" "$PLANO_IOS_BUNDLE_ID" "$evidence" <<'PY'
import collections
from pathlib import Path
import subprocess
import sys

driver, device, app_id, evidence = sys.argv[1:]
destination = Path(evidence)
artifacts = destination / "maestro"
artifacts.mkdir()
command = [
    driver, "test", "--device", device, "-p", "ios", "--no-ansi",
    "--test-output-dir", str(artifacts), "--debug-output", str(destination / "debug"),
    "--format", "JUNIT", "--output", str(destination / "results.xml"),
    "-e", "APP_ID=" + app_id,
    "-e", "SERVER_URL=https://127.0.0.1:8443",
    "-e", "EVIDENCE_DIR=" + str(artifacts),
    "mobile/scripts/ios-demo-flow.yaml",
]
log = destination / "maestro.log"
with log.open("w") as output:
    try:
        completed = subprocess.run(command, stdout=output, stderr=subprocess.STDOUT, timeout=540)
        result = completed.returncode
    except subprocess.TimeoutExpired:
        print("The iOS demo UI flow exceeded nine minutes.")
        result = 124
with log.open() as output:
    print("".join(collections.deque(output, maxlen=80)), end="")
sys.exit(result)
PY
xcrun simctl io "$device" screenshot "$evidence/final-detail.png"
printf '%s\n' 'iOS connected over HTTPS, searched and tracked UA123, and retained it after relaunch.'
