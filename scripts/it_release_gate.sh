#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ -n "${ONPREST_IT_GATE_LOG_DIR:-}" ]]; then
	LOG_DIR="$ONPREST_IT_GATE_LOG_DIR"
	LOG_DIR_CREATED=0
else
	LOG_DIR="$(mktemp -d)"
	LOG_DIR_CREATED=1
fi
KEEP_LOGS="${ONPREST_IT_GATE_KEEP_LOGS:-0}"

assert_prebuilt_distribution() {
	local gateway="${ONPREST_IT_GATEWAY_BINARY:-}"
	local agent="${ONPREST_IT_AGENT_BINARY:-}"
	local version="${ONPREST_IT_DISTRIBUTION_VERSION:-}"
	if [[ -z $gateway && -z $agent ]]; then
		return
	fi
	if [[ -z $gateway || -z $agent || -z $version ]]; then
		echo "gateway, agent, and distribution version must be provided together" >&2
		exit 1
	fi
	for binary in "$gateway" "$agent"; do
		if [[ ! -x $binary ]]; then
			echo "prebuilt distribution binary is missing or not executable: $binary" >&2
			exit 1
		fi
		absolute="$(cd "$(dirname "$binary")" && pwd -P)/$(basename "$binary")"
		if [[ $absolute == "$ROOT_DIR"/* ]]; then
			echo "prebuilt distribution binary must be extracted outside the source tree: $absolute" >&2
			exit 1
		fi
		if [[ $(cd / && "$absolute" --version) != "$version" ]]; then
			echo "prebuilt distribution binary version does not match $version: $absolute" >&2
			exit 1
		fi
	done
	echo "PASS: source-free distribution binaries ($version)"
}

cleanup_logs() {
	if [[ "$LOG_DIR_CREATED" == "1" && "$KEEP_LOGS" != "1" && -d "$LOG_DIR" ]]; then
		rm -rf "$LOG_DIR"
	fi
}
trap cleanup_logs EXIT

assert_docker_available() {
	if ! command -v docker >/dev/null 2>&1; then
		echo "docker command is required for the integration release gate" >&2
		exit 1
	fi
	if ! docker info >/dev/null 2>&1; then
		echo "docker daemon is required for the integration release gate" >&2
		exit 1
	fi
}

assert_no_testcontainers_left() {
	local leftovers=""
	for _ in {1..12}; do
		leftovers="$(docker ps -a --filter label=org.testcontainers=true --format '{{.ID}} {{.Image}} {{.Status}} {{.Names}}')"
		if [[ -z "$leftovers" ]]; then
			echo "PASS: no testcontainers leftovers"
			return
		fi
		sleep 5
	done
	echo "testcontainers leftovers remain:" >&2
	echo "$leftovers" >&2
	exit 1
}

mkdir -p "$LOG_DIR"
assert_prebuilt_distribution
assert_docker_available

# Each named partition runs once. The only repeated case is DB interruption.
# No untagged/schema-tagged unit reruns: ci.yml owns the tagged unit suite.
run_suite() {
  local suite="$1"
  local args=()
  case "$suite" in
    postgres-[1-4])
      args=(-package=./it -tags=integration,schema_validator
        -exclude='^Test(ContainerDBDriver|Docker|MySQL|SQLServerTLS|OracleTLS|PostgresTLS|PostgresDBUnreachableDuringQuery)'
        -shard="${suite#postgres-}" -shards=4 -- -args -onprest-it-db=postgres)
      ;;
    db-postgres)
      args=(-package=./it -tags=integration,schema_validator -include='^Test(ContainerDBDriver|PostgresTLS)' -- -args -onprest-it-db=postgres)
      ;;
    db-mysql)
      args=(-package=./it -tags=integration,schema_validator -include='^Test(ContainerDBDriver|MySQL)' -- -args -onprest-it-db=mysql)
      ;;
    db-sqlserver)
      args=(-package=./it -tags=integration,schema_validator -include='^Test(ContainerDBDriver|SQLServerTLS)' -- -args -onprest-it-db=sqlserver)
      ;;
    db-oracle)
      args=(-package=./it -tags=integration,schema_validator -include='^Test(ContainerDBDriver|OracleTLS)' -- -args -onprest-it-db=oracle)
      ;;
    docker)
      export ONPREST_IT_DOCKER=1 ONPREST_IT_DOCKER_COMPOSE=1
      args=(-package=./it -tags=integration -include='^TestDocker')
      ;;
    stability)
      args=(-package=./it -tags=integration -include='^TestPostgresDBUnreachableDuringQuery$' -- -count=5 -args -onprest-it-db=postgres)
      ;;
    quickstart)
      # This smoke always consumes the candidate archive, outside the checkout.
      : "${ONPREST_IT_GATEWAY_BINARY:?quickstart requires distribution binaries}"
      : "${ONPREST_IT_QUICKSTART_DIR:?quickstart archive is required}"
      bash scripts/quickstart_smoke.sh "$(dirname "$ONPREST_IT_GATEWAY_BINARY")" "$ONPREST_IT_QUICKSTART_DIR" 2>&1 | tee "$LOG_DIR/quickstart.log"
      return
      ;;
    *) echo "unknown integration suite: $suite" >&2; exit 1 ;;
  esac
  ONPREST_IT_REQUIRE_CONTAINERS=1 go run ./internal/citest -log="$LOG_DIR/$suite.jsonl" "${args[@]}"
}

suite="${1:-all}"
if [[ "$suite" == all ]]; then
  # Maintainer convenience only; ordinary local development runs affected tests.
  npm ci --ignore-scripts --no-audit --no-fund --prefix internal/contract/testdata/schema-validator
  for part in postgres-{1..4} db-postgres db-mysql db-sqlserver db-oracle docker stability; do
    run_suite "$part"
  done
  if [[ -n "${ONPREST_IT_GATEWAY_BINARY:-}" ]]; then run_suite quickstart; fi
else
  run_suite "$suite"
fi
assert_no_testcontainers_left
echo "PASS: integration partition $suite"
