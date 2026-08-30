#!/usr/bin/env bash
set -euo pipefail

agent_bin=${1:?agent binary path is required}
gateway_bin=${2:?gateway binary path is required}
config_path=${3:?capability path is required}
agent_bin=$(cd "$(dirname "$agent_bin")" && pwd)/$(basename "$agent_bin")
gateway_bin=$(cd "$(dirname "$gateway_bin")" && pwd)/$(basename "$gateway_bin")
config_path=$(cd "$(dirname "$config_path")" && pwd)/$(basename "$config_path")
artifact_dir=$(dirname "$agent_bin")
default_config=$artifact_dir/capability.yaml
platform=$(uname -s)

file_mode() {
  case "$platform" in
    Darwin) stat -f '%Lp' "$1" ;;
    Linux) stat -c '%a' "$1" ;;
    *)
      echo "unsupported Unix service test platform: $platform" >&2
      return 1
      ;;
  esac
}

file_size() {
  case "$platform" in
    Darwin) stat -f '%z' "$1" ;;
    Linux) stat -c '%s' "$1" ;;
    *)
      echo "unsupported Unix service test platform: $platform" >&2
      return 1
      ;;
  esac
}

if [[ $(id -u) -eq 0 ]]; then
  elevate=()
else
  elevate=(sudo)
fi

gateway_pid=
runtime_writer_pid=
blocking_validate_pid=
blackhole_pid=
lock_holder_pid=

cleanup() {
  for pid in "$lock_holder_pid" "$blocking_validate_pid" "$blackhole_pid" "$runtime_writer_pid"; do
    if [[ -n $pid ]]; then
      "${elevate[@]}" kill "$pid" >/dev/null 2>&1 || kill "$pid" >/dev/null 2>&1 || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  "${elevate[@]}" "$agent_bin" service stop >/dev/null 2>&1 || true
  "${elevate[@]}" "$agent_bin" service uninstall >/dev/null 2>&1 || true
  if [[ -n $gateway_pid ]]; then
    kill "$gateway_pid" >/dev/null 2>&1 || true
    wait "$gateway_pid" 2>/dev/null || true
  fi
  "${elevate[@]}" rm -f \
    "$default_config" \
    "$default_config.invalid.out" \
    "$artifact_dir/capability.rollout-initial.yaml" \
    "$artifact_dir/capability.validate-a.yaml" "$artifact_dir/capability.validate-a.yaml.out" \
    "$artifact_dir/capability.validate-b.yaml" "$artifact_dir/capability.validate-b.yaml.out" \
    "$artifact_dir/capability.validate-blocking.yaml" "$artifact_dir/capability.validate-blocking.yaml.out" "$artifact_dir/capability.validate-blocking.yaml.err" \
    "$artifact_dir/rollout-before-restart.json" "$artifact_dir/service-test-gateway.jsonl" \
    "$artifact_dir/.validate-lock-held" "$artifact_dir/.validate-lock-held.out" \
    "$artifact_dir/onprest-agent.validate.log" "$artifact_dir/.onprest-agent.validate.lock" \
    "$agent_bin.log" "$agent_bin.log.1" "$agent_bin.log.2" >/dev/null 2>&1 || true
  "${elevate[@]}" find "$artifact_dir" -maxdepth 1 -type f -name '.onprest-agent.validate.*.tmp' -delete >/dev/null 2>&1 || true
}

# Remove a stale native test service before creating this run's secrets and
# processes. Do not invoke the final cleanup here: it also owns the Gateway
# started below.
"${elevate[@]}" "$agent_bin" service stop >/dev/null 2>&1 || true
"${elevate[@]}" "$agent_bin" service uninstall >/dev/null 2>&1 || true
trap cleanup EXIT

agent_secret=$($gateway_bin create-agent-secret)
agent_private_key=$(jq -r .agent_private_key <<<"$agent_secret")
agent_public_key=$(jq -r .agent_public_key <<<"$agent_secret")
sed "s|PRIVATE_KEY|$agent_private_key|" "$config_path" > "$default_config"
api_secret=$($gateway_bin create-key --name service-test --capabilities runtime_marker_a,runtime_marker_b,rollout_marker_new)
api_key=$(jq -r .api_key <<<"$api_secret")
api_keys_json=$(jq -c '[{name:.name,key_hash:.key_hash,capabilities:.capabilities}]' <<<"$api_secret")
gateway_output=$(dirname "$agent_bin")/service-test-gateway.jsonl
GATEWAY_ADDR=127.0.0.1:18080 GATEWAY_AGENT_PUBLIC_KEY=$agent_public_key GATEWAY_API_KEYS_JSON=$api_keys_json \
  GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=1000 GATEWAY_RATE_LIMIT_BURST=1000 \
  "$gateway_bin" >"$gateway_output" 2>&1 &
gateway_pid=$!
for _ in {1..100}; do
  curl -fsS http://127.0.0.1:18080/healthz >/dev/null 2>&1 && break
  sleep 0.05
done
curl -fsS http://127.0.0.1:18080/healthz >/dev/null
kill -0 "$gateway_pid"

dump_systemd_diagnostics() {
  echo '--- onprest-agent.service ---' >&2
  "${elevate[@]}" systemctl cat onprest-agent.service --no-pager >&2 || true
  echo '--- systemd-analyze verify ---' >&2
  "${elevate[@]}" systemd-analyze verify /etc/systemd/system/onprest-agent.service >&2 || true
  echo '--- systemctl status ---' >&2
  "${elevate[@]}" systemctl status onprest-agent.service --no-pager --full >&2 || true
  echo '--- journalctl ---' >&2
  "${elevate[@]}" journalctl -u onprest-agent.service --no-pager -n 100 >&2 || true
}

start_service() {
  if "${elevate[@]}" "$agent_bin" service start; then
    return
  fi
  if [[ $(uname -s) == Linux ]]; then
    dump_systemd_diagnostics
  fi
  return 1
}

"${elevate[@]}" "$agent_bin" service install
installed=$("${elevate[@]}" "$agent_bin" service status)
grep -q '^installed: true$' <<<"$installed"
grep -Fq "config: $default_config" <<<"$installed"
grep -Fq "binary: $agent_bin" <<<"$installed"
case "$platform" in
  Linux)
    grep -q '^native: systemd$' <<<"$installed"
    if ! "${elevate[@]}" systemd-analyze verify /etc/systemd/system/onprest-agent.service; then
      dump_systemd_diagnostics
      exit 1
    fi
    if systemctl is-enabled onprest-agent.service >/dev/null 2>&1; then
      echo 'service install unexpectedly enabled boot-time start' >&2
      exit 1
    fi
    ;;
  Darwin)
    grep -q '^native: launchd$' <<<"$installed"
    grep -A1 '<key>RunAtLoad</key>' '/Library/Application Support/Onprest/com.onprest.agent.plist' | grep -q '<false/>'
    if launchctl print system/com.onprest.agent >/dev/null 2>&1; then
      echo 'service install unexpectedly loaded the launch daemon' >&2
      exit 1
    fi
    ;;
  *)
    echo "unsupported Unix service test platform: $platform" >&2
    exit 1
    ;;
esac

start_service
for _ in {1..40}; do
  running=$("${elevate[@]}" "$agent_bin" service status)
  if grep -Eq '^state: (active|running)$' <<<"$running"; then
    break
  fi
  sleep 0.25
done
grep -Eq '^state: (active|running)$' <<<"$running"
grep -Eq '^pid: [1-9][0-9]*$' <<<"$running"
sleep 1
still_running=$("${elevate[@]}" "$agent_bin" service status)
grep -Eq '^state: (active|running)$' <<<"$still_running"

for _ in {1..100}; do
  curl -fsS http://127.0.0.1:18080/healthz | grep -q '"agent_connected":true' && break
  sleep 0.05
done
curl -fsS http://127.0.0.1:18080/healthz | grep -q '"agent_connected":true'

assert_old_public_contract() {
  old_rest=$(curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
    -d '{"runtime_marker_a":"service-old"}' http://127.0.0.1:18080/api/v1/capabilities/runtime_marker_a)
  jq -e '.count == 1 and .rows[0].value == "service-old"' <<<"$old_rest" >/dev/null
  old_mcp=$(curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":"old-call","method":"tools/call","params":{"name":"runtime_marker_a","arguments":{"runtime_marker_a":"service-old"}}}' \
    http://127.0.0.1:18080/mcp)
  jq -e '.result.isError != true and .result.structuredContent.count == 1 and .result.structuredContent.rows[0].value == "service-old"' <<<"$old_mcp" >/dev/null
  old_tools=$(curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":"old-list","method":"tools/list"}' http://127.0.0.1:18080/mcp)
  jq -e '[.result.tools[].name] | index("runtime_marker_a") != null' <<<"$old_tools" >/dev/null
  old_openapi=$(curl -fsS -H "Authorization: Bearer $api_key" http://127.0.0.1:18080/openapi.json)
  jq -e '.paths["/api/v1/capabilities/runtime_marker_a"] != null' <<<"$old_openapi" >/dev/null
}

assert_new_capability_absent() {
  current_tools=$(curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":"before-restart-list","method":"tools/list"}' http://127.0.0.1:18080/mcp)
  jq -e '[.result.tools[].name] | index("rollout_marker_new") == null' <<<"$current_tools" >/dev/null
  current_openapi=$(curl -fsS -H "Authorization: Bearer $api_key" http://127.0.0.1:18080/openapi.json)
  jq -e '.paths["/api/v1/capabilities/rollout_marker_new"] == null' <<<"$current_openapi" >/dev/null
  new_status=$(curl -sS -o "$(dirname "$agent_bin")/rollout-before-restart.json" -w '%{http_code}' \
    -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' -d '{}' \
    http://127.0.0.1:18080/api/v1/capabilities/rollout_marker_new)
  test "$new_status" = 404
}

# Exercise the documented capability rollout through the actual service
# manager. An invalid edit must leave the running service and its old public
# REST/MCP/OpenAPI contract intact. A valid edit is only loaded after stop/start.
assert_old_public_contract
assert_new_capability_absent
initial_config=$(dirname "$agent_bin")/capability.rollout-initial.yaml
cp "$default_config" "$initial_config"
printf '\ninvalid_rollout_field: true\n' >> "$default_config"
if "${elevate[@]}" "$agent_bin" validate --config "$default_config" --format json >"$default_config.invalid.out"; then
  echo 'validate unexpectedly accepted the invalid rollout edit' >&2
  exit 1
fi
grep -q '"stage":"config"' "$default_config.invalid.out"
invalid_status=$("${elevate[@]}" "$agent_bin" service status)
grep -Eq '^state: (active|running)$' <<<"$invalid_status"
assert_old_public_contract
assert_new_capability_absent

cp "$initial_config" "$default_config"
{
  printf '\n'
  printf '%s\n' '  rollout_marker_new:'
  printf '%s\n' "    sql: select 'rollout-v2' as value"
  printf '%s\n' '    policy:'
  printf '%s\n' '      readonly: true'
  printf '%s\n' '    result:'
  printf '%s\n' '      value:'
  printf '%s\n' '        type: string'
} >> "$default_config"
"${elevate[@]}" "$agent_bin" validate --config "$default_config" --format json | grep -q '"valid":true'
pre_restart_status=$("${elevate[@]}" "$agent_bin" service status)
grep -Eq '^state: (active|running)$' <<<"$pre_restart_status"
assert_old_public_contract
assert_new_capability_absent

"${elevate[@]}" "$agent_bin" service stop
for _ in {1..40}; do
  rollout_stopped=$("${elevate[@]}" "$agent_bin" service status)
  if ! grep -Eq '^state: (active|running)$' <<<"$rollout_stopped"; then break; fi
  sleep 0.25
done
if grep -Eq '^state: (active|running)$' <<<"$rollout_stopped"; then
  echo "service did not stop for capability rollout: $rollout_stopped" >&2
  exit 1
fi
start_service
for _ in {1..100}; do
  rollout_openapi=$(curl -fsS -H "Authorization: Bearer $api_key" http://127.0.0.1:18080/openapi.json 2>/dev/null || true)
  if jq -e '.paths["/api/v1/capabilities/rollout_marker_new"] != null' <<<"$rollout_openapi" >/dev/null 2>&1; then break; fi
  sleep 0.05
done
jq -e '.paths["/api/v1/capabilities/rollout_marker_new"] != null' <<<"$rollout_openapi" >/dev/null
new_rest=$(curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' -d '{}' \
  http://127.0.0.1:18080/api/v1/capabilities/rollout_marker_new)
jq -e '.count == 1 and .rows[0].value == "rollout-v2"' <<<"$new_rest" >/dev/null
new_tools=$(curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":"new-list","method":"tools/list"}' http://127.0.0.1:18080/mcp)
jq -e '[.result.tools[].name] | index("rollout_marker_new") != null' <<<"$new_tools" >/dev/null
new_mcp=$(curl -fsS -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":"new-call","method":"tools/call","params":{"name":"rollout_marker_new","arguments":{}}}' \
  http://127.0.0.1:18080/mcp)
jq -e '.result.isError != true and .result.structuredContent.count == 1 and .result.structuredContent.rows[0].value == "rollout-v2"' <<<"$new_mcp" >/dev/null
rm -f "$initial_config" "$default_config.invalid.out" "$(dirname "$agent_bin")/rollout-before-restart.json"

runtime_request() {
  curl -sS -o /dev/null -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \
    -d '{}' "http://127.0.0.1:18080/api/v1/capabilities/$1"
}

# Drive the running production Agent through the real Gateway so its runtime
# detail writer receives multiple safe validation records and rotates them.
runtime_request runtime_marker_a
runtime_request runtime_marker_b

# validate uses the production startup preflight while the installed Agent is
# still running. Its latest-failure files must never touch runtime rotation.
"${elevate[@]}" "$agent_bin" validate --config "$default_config" --format json | grep -q '"valid":true'
runtime_log="$agent_bin.log"
test -f "$runtime_log"
test -f "$runtime_log.1"
"${elevate[@]}" grep -q 'runtime_marker_b' "$runtime_log"
"${elevate[@]}" grep -q 'runtime_marker_a' "$runtime_log.1"

# Keep writing runtime records while production validate runs. The markers
# must remain in runtime generations and no rename/close failure may stop the
# installed Agent.
(for _ in {1..10}; do runtime_request runtime_marker_a; runtime_request runtime_marker_b; done) &
runtime_writer_pid=$!
"${elevate[@]}" "$agent_bin" validate --config "$default_config" --format json | grep -q '"valid":true'
wait "$runtime_writer_pid"
runtime_writer_pid=
"${elevate[@]}" grep -q 'runtime_marker_b' "$runtime_log"
"${elevate[@]}" grep -q 'runtime_marker_a' "$runtime_log.1"
post_rotation_status=$("${elevate[@]}" "$agent_bin" service status)
grep -Eq '^state: (active|running)$' <<<"$post_rotation_status"
runtime_before=$("${elevate[@]}" cksum "$runtime_log" "$runtime_log.1")

invalid_a=$(dirname "$agent_bin")/capability.validate-a.yaml
invalid_b=$(dirname "$agent_bin")/capability.validate-b.yaml
sed 's/name: postgres/name: validate_missing_database_a/' "$default_config" > "$invalid_a"
sed 's/name: postgres/name: validate_missing_database_b/' "$default_config" > "$invalid_b"
if "${elevate[@]}" "$agent_bin" validate --config "$invalid_a" --format json >"$invalid_a.out"; then
  echo 'validate unexpectedly accepted missing database A' >&2
  exit 1
fi
grep -q '"stage":"database_ping"' "$invalid_a.out"
fixed_log=$(dirname "$agent_bin")/onprest-agent.validate.log
test -f "$fixed_log"
"${elevate[@]}" grep -q 'validate_missing_database_a' "$fixed_log"
if "${elevate[@]}" "$agent_bin" validate --config "$invalid_b" --format json >"$invalid_b.out"; then
  echo 'validate unexpectedly accepted missing database B' >&2
  exit 1
fi
grep -q '"stage":"database_ping"' "$invalid_b.out"
"${elevate[@]}" grep -q 'validate_missing_database_b' "$fixed_log"
if "${elevate[@]}" grep -q 'validate_missing_database_a' "$fixed_log"; then
  echo 'validate retained an older failure' >&2
  exit 1
fi
test "$(file_mode "$fixed_log")" = 600
lock_file=$(dirname "$agent_bin")/.onprest-agent.validate.lock
test -f "$lock_file"
test "$(file_mode "$lock_file")" = 600
test "$(file_size "$lock_file")" = 0

# Hold a production validate inside DB Ping long enough to inspect its live
# temporary file. It must be private from the instant it is created.
blocking_config=$(dirname "$agent_bin")/capability.validate-blocking.yaml
sed 's/port: 5432/port: 18081/' "$default_config" > "$blocking_config"
perl -MIO::Socket::INET -e '$s=IO::Socket::INET->new(LocalAddr=>"127.0.0.1",LocalPort=>18081,Listen=>1,ReuseAddr=>1) or die $!; $c=$s->accept; sleep 15' &
blackhole_pid=$!
sleep 0.2
"${elevate[@]}" "$agent_bin" validate --config "$blocking_config" --format json >"$blocking_config.out" 2>"$blocking_config.err" &
blocking_validate_pid=$!
temporary_log=
for _ in {1..100}; do
  temporary_log=$(find "$(dirname "$agent_bin")" -maxdepth 1 -type f -name '.onprest-agent.validate.*.tmp' -print -quit)
  [[ -n $temporary_log ]] && break
  sleep 0.05
done
[[ -n $temporary_log ]]
test "$(file_mode "$temporary_log")" = 600
"${elevate[@]}" kill "$blocking_validate_pid"
wait "$blocking_validate_pid" 2>/dev/null || true
blocking_validate_pid=
kill "$blackhole_pid" >/dev/null 2>&1 || true
wait "$blackhole_pid" 2>/dev/null || true
blackhole_pid=

# Hold the same native advisory lock from a separate process. Production
# validate must report busy without inspecting the config or changing either
# the completed fixed log or temporary-file set.
lock_marker=$(dirname "$agent_bin")/.validate-lock-held
rm -f "$lock_marker"
"${elevate[@]}" perl -e 'open(my $f, "+<", $ARGV[0]) or die $!; flock($f, 2|4) or die $!; open(my $m, ">", $ARGV[1]) or die $!; close($m); sleep 30' "$lock_file" "$lock_marker" &
lock_holder_pid=$!
for _ in {1..100}; do
  test -e "$lock_marker" && break
  sleep 0.05
done
test -e "$lock_marker"
fixed_before_busy=$("${elevate[@]}" cksum "$fixed_log")
temporary_before_busy=$(find "$(dirname "$agent_bin")" -maxdepth 1 -type f -name '.onprest-agent.validate.*.tmp' -print | sort)
if "${elevate[@]}" "$agent_bin" validate --config "$default_config" --format json >"$lock_marker.out"; then
  echo 'second validate unexpectedly acquired the live native lock' >&2
  exit 1
fi
grep -q '"stage":"busy"' "$lock_marker.out"
test "$fixed_before_busy" = "$("${elevate[@]}" cksum "$fixed_log")"
test "$temporary_before_busy" = "$(find "$(dirname "$agent_bin")" -maxdepth 1 -type f -name '.onprest-agent.validate.*.tmp' -print | sort)"
"${elevate[@]}" kill "$lock_holder_pid"
wait "$lock_holder_pid" 2>/dev/null || true
lock_holder_pid=
rm -f "$lock_marker" "$lock_marker.out"

"${elevate[@]}" "$agent_bin" validate --config "$default_config" >/dev/null
test ! -e "$fixed_log"
runtime_after=$("${elevate[@]}" cksum "$runtime_log" "$runtime_log.1")
test "$runtime_before" = "$runtime_after"
rm -f "$invalid_a" "$invalid_b" "$invalid_a.out" "$invalid_b.out" "$blocking_config" "$blocking_config.out" "$blocking_config.err"

"${elevate[@]}" "$agent_bin" service stop
for _ in {1..40}; do
  stopped=$("${elevate[@]}" "$agent_bin" service status)
  if ! grep -Eq '^state: (active|running)$' <<<"$stopped"; then
    break
  fi
  sleep 0.25
done
if grep -Eq '^state: (active|running)$' <<<"$stopped"; then
  echo "service did not stop: $stopped" >&2
  exit 1
fi

"${elevate[@]}" "$agent_bin" service remove
removed=$("${elevate[@]}" "$agent_bin" service status)
grep -q '^installed: false$' <<<"$removed"
cleanup
trap - EXIT
