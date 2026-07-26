#!/usr/bin/env bash
set -euo pipefail

agent_bin=${1:?agent binary path is required}
config_path=${2:?capability path is required}
agent_bin=$(cd "$(dirname "$agent_bin")" && pwd)/$(basename "$agent_bin")
config_path=$(cd "$(dirname "$config_path")" && pwd)/$(basename "$config_path")
default_config=$(dirname "$agent_bin")/capability.yaml
cp "$config_path" "$default_config"

if [[ $(id -u) -eq 0 ]]; then
  elevate=()
else
  elevate=(sudo)
fi

cleanup() {
  "${elevate[@]}" "$agent_bin" service stop >/dev/null 2>&1 || true
  "${elevate[@]}" "$agent_bin" service uninstall >/dev/null 2>&1 || true
}

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

trap cleanup EXIT
cleanup

"${elevate[@]}" "$agent_bin" service install
installed=$("${elevate[@]}" "$agent_bin" service status)
grep -q '^installed: true$' <<<"$installed"
grep -Fq "config: $default_config" <<<"$installed"
grep -Fq "binary: $agent_bin" <<<"$installed"
case "$(uname -s)" in
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
    echo "unsupported Unix service test platform: $(uname -s)" >&2
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
trap - EXIT
