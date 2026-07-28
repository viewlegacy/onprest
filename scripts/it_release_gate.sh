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

cleanup_logs() {
	if [[ "$LOG_DIR_CREATED" == "1" && "$KEEP_LOGS" != "1" && -d "$LOG_DIR" ]]; then
		rm -rf "$LOG_DIR"
	fi
}
trap cleanup_logs EXIT

run_go_json_no_skip() {
	local label="$1"
	shift
	local log="$LOG_DIR/${label}.jsonl"
	echo "==> $label"
	set +e
	"$@" >"$log" 2>&1
	local status=$?
	set -e
	# Package-level skip records have no Test field and are emitted for packages
	# without test files. Only an actual test/subtest skip invalidates the gate.
	if grep -Eq '"Action":"skip".*"Test":"[^"]+"' "$log"; then
		echo "skip detected in $label; full log: $log" >&2
		grep -E '"Action":"skip".*"Test":"[^"]+"' "$log" >&2 || true
		exit 1
	fi
	if [[ "$status" -ne 0 ]]; then
		echo "$label failed with exit status $status; full log: $log" >&2
		tail -n 200 "$log" >&2 || true
		exit "$status"
	fi
	echo "PASS: $label"
}

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
echo "integration release gate logs: $LOG_DIR"

assert_docker_available

echo "==> govulncheck"
make vulncheck
echo "PASS: govulncheck"

run_go_json_no_skip "unit" go test -json ./...

run_go_json_no_skip "postgres-ci" \
	env ONPREST_IT_REQUIRE_CONTAINERS=1 \
	go test -json -tags=integration ./it/... -skip '^TestDocker' -count=3 -args -onprest-it-db=postgres

run_go_json_no_skip "postgres-db-interruption-stability" \
	env ONPREST_IT_REQUIRE_CONTAINERS=1 \
	go test -json -tags=integration ./it/... -run '^TestPostgresDBUnreachableDuringQuery$' -count=5 -args -onprest-it-db=postgres

run_go_json_no_skip "all-db-smoke" \
	env ONPREST_IT_REQUIRE_CONTAINERS=1 \
	go test -json -tags=integration ./it/... -run '^TestContainerDBDriver' -timeout 30m -count=1 -args -onprest-it-db=all

run_go_json_no_skip "mysql-special-credentials" \
	env ONPREST_IT_REQUIRE_CONTAINERS=1 \
	go test -json -tags=integration ./it/... -run '^TestMySQLDSNSpecialCredentialsConnectToRealDatabase$' -timeout 10m -count=1 -args -onprest-it-db=mysql

run_go_json_no_skip "sqlserver-tls" \
	env ONPREST_IT_REQUIRE_CONTAINERS=1 \
	go test -json -tags=integration ./it/... -run '^TestSQLServerTLSRequireAndVerifyFullAgainstRealDatabase$' -timeout 10m -count=1 -args -onprest-it-db=sqlserver

run_go_json_no_skip "docker-image-ops" \
	env ONPREST_IT_DOCKER=1 \
	go test -json -tags=integration ./it/... -run '^TestDockerTargetsBuildWhenDockerIntegrationEnabled$' -count=1

run_go_json_no_skip "docker-compose-ops" \
	env ONPREST_IT_DOCKER_COMPOSE=1 \
	go test -json -tags=integration ./it/... -run '^TestDockerComposeEnvFilePreservesGatewayAPIKeysJSON$' -count=1

assert_no_testcontainers_left

echo "PASS: integration release gate"
