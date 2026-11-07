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
activity_probe=${PLANO_IOS_ACTIVITY_PROBE:-false}
case "$activity_probe" in
  true|false) ;;
  *) printf '%s\n' 'PLANO_IOS_ACTIVITY_PROBE must be true or false.' >&2; exit 1 ;;
esac
mkdir -p "$evidence/activity"
if [[ "$activity_probe" == true ]]; then
  printf '%s\n' '{"status":"UNVERIFIED","reason":"base-flow-not-complete","deadline_seconds":360}' >"$evidence/activity/status.json"
else
  printf '%s\n' '{"status":"UNVERIFIED","reason":"not-requested"}' >"$evidence/activity/status.json"
fi

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

if ! selected_device=$(xcrun simctl list devices available --json | jq -ce --arg probe "$activity_probe" '
  [.devices | to_entries[]
   | select(.key | startswith("com.apple.CoreSimulator.SimRuntime.iOS-26-4"))
   | .key as $runtime | .value[]
   | select(.isAvailable and .state == "Shutdown" and (.name | startswith("iPhone")))
   | select($probe != "true" or (.name | test("^iPhone (14|15|16|17) Pro( Max)?$")))
   | {udid, name, runtime: $runtime}]
  | .[0] // error("No suitable iOS 26.4 iPhone simulator is available.")'); then
  if [[ "$activity_probe" == true ]]; then
    printf '%s\n' '{"status":"UNVERIFIED","reason":"no-dynamic-island-simulator"}' >"$evidence/activity/status.json"
  fi
  printf '%s\n' 'No suitable iOS 26.4 iPhone simulator is available; the activity probe requires a known Dynamic Island Pro model.' >&2
  exit 1
fi
printf '%s\n' "$selected_device" >"$evidence/device.json"
device=$(jq -er '.udid | strings' <<<"$selected_device")
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

python3 - "$work/maestro/bin/maestro" "$device" "$PLANO_IOS_BUNDLE_ID" "$evidence" "$activity_probe" <<'PY'
import collections
import json
import os
from pathlib import Path
import subprocess
import sys

driver, device, app_id, evidence, activity_probe = sys.argv[1:]
destination = Path(evidence)
activity = destination / "activity"
activity_deadline = 360
selected = json.loads((destination / "device.json").read_text())

def activity_status(reason, result=None):
    (activity / "status.json").write_text(json.dumps({
        "status": "UNVERIFIED", "reason": reason, "cli_exit": result,
        "deadline_seconds": activity_deadline,
        "source_commit": os.environ.get("GITHUB_SHA"),
        "device_name": selected["name"], "runtime": selected["runtime"],
        "evidence": {"junit": "activity/results.xml", "screenshots": "activity/maestro", "final": "activity/final.png"},
    }, indent=2) + "\n")

def run_flow(flow, output_directory, timeout, parameters=(), environment=None):
    artifacts = output_directory / "maestro"
    artifacts.mkdir()
    command = [
        driver, "test", "--device", device, "-p", "ios", "--no-ansi",
        "--test-output-dir", str(artifacts), "--debug-output", str(output_directory / "debug"),
        "--format", "JUNIT", "--output", str(output_directory / "results.xml"),
        "-e", "APP_ID=" + app_id, *parameters, flow,
    ]
    log = output_directory / "maestro.log"
    with log.open("w") as output:
        try:
            completed = subprocess.run(command, stdout=output, stderr=subprocess.STDOUT, timeout=timeout, env=environment)
            result = completed.returncode
        except subprocess.TimeoutExpired:
            print(f"The iOS flow exceeded its {timeout}-second deadline: {flow}")
            result = 124
        except OSError:
            print(f"The iOS flow driver could not start: {flow}")
            result = 127
    with log.open() as output:
        print("".join(collections.deque(output, maxlen=80)), end="")
    return result

result = run_flow("mobile/scripts/ios-demo-flow.yaml", destination, 540,
                  ("-e", "SERVER_URL=https://127.0.0.1:8443"))
if result != 0:
    if activity_probe == "true":
        activity_status("base-flow-failed")
    sys.exit(result)

try:
    subprocess.run(["xcrun", "simctl", "io", device, "screenshot", str(destination / "final-detail.png")],
                   check=True, timeout=10)
except (subprocess.SubprocessError, OSError):
    if activity_probe == "true":
        activity_status("base-evidence-capture-failed")
    raise
print("iOS connected over HTTPS, searched and tracked UA123, and retained it after relaunch.")
if activity_probe != "true":
    sys.exit(0)

activity_status("probe-running")
probe_environment = os.environ.copy()
probe_environment["JAVA_TOOL_OPTIONS"] = os.environ["JAVA_TOOL_OPTIONS"].replace("-Xmx512m", "-Xmx256m", 1)
result = run_flow("mobile/scripts/ios-activity-probe.yaml", activity, activity_deadline, environment=probe_environment)
activity_status("native-surface-review-pending" if result == 0 else "activity-flow-failed", result)
if result == 124:
    activity_status("activity-flow-timeout", result)
# Also retain the actual UI when the driver times out before its completion hook.
try:
    subprocess.run(["xcrun", "simctl", "io", device, "screenshot", str(activity / "final.png")],
                   check=True, timeout=10)
except (subprocess.SubprocessError, OSError):
    if result == 0:
        activity_status("activity-evidence-capture-failed", result)
        result = 1
if result == 0:
    print("iOS Live Activity probe completed; native surface review is pending (UNVERIFIED).")
else:
    print("iOS Live Activity remains UNVERIFIED; inspect activity/results.xml and its UI evidence.")
sys.exit(result)
PY
