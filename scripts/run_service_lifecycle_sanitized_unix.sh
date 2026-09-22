#!/usr/bin/env bash
set -euo pipefail

runtime_cwd=${1:?runtime cwd is required}
agent_bin=${2:?agent binary is required}
gateway_bin=${3:?gateway binary is required}
capability_file=${4:?capability file is required}
original_path=$PATH
runtime_path=$(mktemp -d)
harness_path="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test_service_lifecycle_unix.sh"
trap 'rm -rf "$runtime_path"' EXIT

# The harness needs ordinary OS inspection/network utilities, while the product
# path must not inherit the checked-out Go toolchain or other development tools.
allowed_tools=(
  bash basename cat chmod cksum cp curl cut dirname env find flock grep head id
  journalctl jq kill launchctl mkdir mktemp mv perl rm sed sleep stat sudo
  sort systemctl systemd-analyze tail touch tr uname wc
)
for tool in "${allowed_tools[@]}"; do
  path=$(PATH=$original_path command -v "$tool" || true)
  if [[ -n $path ]]; then ln -s "$path" "$runtime_path/$tool"; fi
done
export PATH=$runtime_path
for tool in go git make docker; do
  if command -v "$tool" >/dev/null 2>&1; then
    echo "development tool remains on service test PATH: $tool" >&2
    exit 1
  fi
done

mkdir -p "$runtime_cwd"
cd "$runtime_cwd"
"$runtime_path/bash" "$harness_path" \
  "$agent_bin" "$gateway_bin" "$capability_file"
