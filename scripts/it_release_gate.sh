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

assert_prebuilt_distribution
assert_docker_available

if [[ -n "${ONPREST_IT_GATEWAY_BINARY:-}" ]]; then
	if [[ -z "${ONPREST_IT_QUICKSTART_DIR:-}" ]]; then
		echo "ONPREST_IT_QUICKSTART_DIR is required with distribution binaries" >&2
		exit 1
	fi
	bash scripts/quickstart_smoke.sh "$(dirname "$ONPREST_IT_GATEWAY_BINARY")" "$ONPREST_IT_QUICKSTART_DIR"
fi

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

run_go_json_no_skip "all-db-conformance" \
	env ONPREST_IT_REQUIRE_CONTAINERS=1 \
	go test -json -tags=integration ./it/... -run '^TestContainerDBDriver' -timeout 30m -count=1 -args -onprest-it-db=all

run_go_json_no_skip "postgres-tls-contract" \
	env ONPREST_IT_REQUIRE_CONTAINERS=1 \
	go test -json -tags=integration ./it/... -run '^TestPostgresTLSModesPrivateCAClientCertificateAndHostnameVerification$' -timeout 10m -count=1 -args -onprest-it-db=postgres

run_go_json_no_skip "mysql-special-credentials" \
	env ONPREST_IT_REQUIRE_CONTAINERS=1 \
	go test -json -tags=integration ./it/... -run '^TestMySQLDSNSpecialCredentialsConnectToRealDatabase$' -timeout 10m -count=1 -args -onprest-it-db=mysql

run_go_json_no_skip "mysql-tls-contract" \
	env ONPREST_IT_REQUIRE_CONTAINERS=1 \
	go test -json -tags=integration ./it/... -run '^TestMySQLTLSModesClientCertificateRotationAndReconnectAgainstRealDatabase$' -timeout 10m -count=1 -args -onprest-it-db=mysql

run_go_json_no_skip "sqlserver-tls" \
	env ONPREST_IT_REQUIRE_CONTAINERS=1 \
	go test -json -tags=integration ./it/... -run '^TestSQLServerTLSRequireAndVerifyFullAgainstRealDatabase$' -timeout 10m -count=1 -args -onprest-it-db=sqlserver

run_go_json_no_skip "oracle-tls-contract" \
	env ONPREST_IT_REQUIRE_CONTAINERS=1 \
	go test -json -tags=integration ./it/... -run '^TestOracleTLSModesVerificationRotationAndReconnectAgainstRealDatabase$' -timeout 15m -count=1 -args -onprest-it-db=oracle

run_go_json_no_skip "docker-image-ops" \
	env ONPREST_IT_DOCKER=1 \
	go test -json -tags=integration ./it/... -run '^TestDockerTargetsBuildWhenDockerIntegrationEnabled$' -count=1

run_go_json_no_skip "docker-compose-ops" \
	env ONPREST_IT_DOCKER_COMPOSE=1 \
	go test -json -tags=integration ./it/... -run '^TestDockerComposeEnvFilePreservesGatewayAPIKeysJSON$' -count=1

assert_no_testcontainers_left

echo "PASS: integration release gate"
