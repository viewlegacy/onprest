#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR=${1:?usage: quickstart_smoke.sh EXTRACTED_BINARY_DIRECTORY EXTRACTED_QUICKSTART_DIRECTORY}
QUICKSTART=${2:?usage: quickstart_smoke.sh EXTRACTED_BINARY_DIRECTORY EXTRACTED_QUICKSTART_DIRECTORY}
INSTALL_DIR=$(cd "$INSTALL_DIR" && pwd -P)
QUICKSTART=$(cd "$QUICKSTART" && pwd -P)
GATEWAY="$INSTALL_DIR/onprest-gateway"
AGENT="$INSTALL_DIR/onprest-agent"
PROJECT="onprest-quickstart-$$"
LOG_DIR=$(mktemp -d)
GATEWAY_PID=""
AGENT_PID=""

for required in \
	"$GATEWAY" \
	"$AGENT" \
	"$QUICKSTART/gateway.env" \
	"$QUICKSTART/capability.postgres.yaml" \
	"$QUICKSTART/postgres.compose.yml" \
	"$QUICKSTART/postgres-init.sql"; do
	if [[ ! -s $required ]]; then
		echo "quickstart file is missing or empty: $required" >&2
		exit 1
	fi
done
if [[ ! -x $GATEWAY || ! -x $AGENT ]]; then
	echo "quickstart binaries must be executable" >&2
	exit 1
fi

cleanup() {
	status=$?
	trap - EXIT INT TERM
	for pid in "$AGENT_PID" "$GATEWAY_PID"; do
		if [[ -n $pid ]]; then
			kill "$pid" >/dev/null 2>&1 || true
			wait "$pid" >/dev/null 2>&1 || true
		fi
	done
	docker compose -p "$PROJECT" -f "$QUICKSTART/postgres.compose.yml" down -v --remove-orphans >/dev/null 2>&1 || true
	if [[ $status -ne 0 ]]; then
		for log in "$LOG_DIR"/*.log; do
			if [[ -f $log ]]; then
				echo "==> $log" >&2
				tail -n 100 "$log" >&2 || true
			fi
		done
	fi
	rm -rf "$LOG_DIR"
	exit "$status"
}
trap cleanup EXIT INT TERM

docker compose -p "$PROJECT" -f "$QUICKSTART/postgres.compose.yml" up -d --wait
"$AGENT" validate --config "$QUICKSTART/capability.postgres.yaml"

(
	cd "$INSTALL_DIR"
	set -a
	. "$QUICKSTART/gateway.env"
	set +a
	export PATH=/usr/bin:/bin
	exec "$GATEWAY"
) >"$LOG_DIR/gateway.log" 2>&1 &
GATEWAY_PID=$!

(
	cd "$INSTALL_DIR"
	export PATH=/usr/bin:/bin
	exec "$AGENT" --config "$QUICKSTART/capability.postgres.yaml"
) >"$LOG_DIR/agent.log" 2>&1 &
AGENT_PID=$!

health=""
for _ in {1..60}; do
	health=$(curl -fsS http://127.0.0.1:8080/healthz 2>/dev/null || true)
	if [[ ${health//[[:space:]]/} == *'"agent_connected":true'* ]]; then
		break
	fi
	sleep 1
done
if [[ ${health//[[:space:]]/} != *'"agent_connected":true'* ]]; then
	echo "quickstart agent did not connect: $health" >&2
	exit 1
fi

API_KEY='orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM'
rest=$(curl -fsS \
	-H "Authorization: Bearer $API_KEY" \
	-H 'Content-Type: application/json' \
	-d '{"customer_id":1}' \
	http://127.0.0.1:8080/api/v1/capabilities/get_customer)
if [[ $rest != *'Ada Lovelace'* ]]; then
	echo "quickstart REST response mismatch: $rest" >&2
	exit 1
fi

mcp=$(curl -fsS \
	-H "Authorization: Bearer $API_KEY" \
	-H 'Content-Type: application/json' \
	-H 'MCP-Protocol-Version: 2025-11-25' \
	-H 'Accept: application/json, text/event-stream' \
	-d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
	http://127.0.0.1:8080/mcp)
if [[ $mcp != *'get_customer'* || $mcp != *'update_customer'* ]]; then
	echo "quickstart MCP response mismatch: $mcp" >&2
	exit 1
fi

echo "PASS: extracted release Quick Start"
