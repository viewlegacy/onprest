#!/usr/bin/env bash
set -euo pipefail
# Development verification of the published two-binary example. No DB needed.
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
binary_dir=${BIN_DIR:-"$repo_dir/dist"}
evidence_dir=${ONPREST_EXAMPLE_EVIDENCE_DIR:-$(mktemp -d /tmp/onprest-file-input.XXXXXX)}
mkdir -p "$evidence_dir/exchange"
cp "$repo_dir/examples/file-input/agent.yaml" "$evidence_dir/agent.yaml"
cp "$repo_dir/examples/file-input/gateway.env" "$evidence_dir/gateway.env"
port=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
python3 - "$evidence_dir/agent.yaml" "$port" <<'PY'
from pathlib import Path
import sys
p=Path(sys.argv[1]);p.write_text(p.read_text().replace('127.0.0.1:8080','127.0.0.1:'+sys.argv[2]))
PY
"$binary_dir/onprest-agent" validate --config "$evidence_dir/agent.yaml" > "$evidence_dir/validate.log" 2>&1
[ ! -e "$evidence_dir/spool" ]
[ -z "$(ls -A "$evidence_dir/exchange")" ]
set -a
. "$evidence_dir/gateway.env"
GATEWAY_ADDR="127.0.0.1:$port"
ONPREST_PUBLIC_BASE_URL="http://127.0.0.1:$port"
set +a
"$binary_dir/onprest-gateway" > "$evidence_dir/gateway.log" 2>&1 &
gateway_pid=$!
agent_pid=
cleanup(){ kill ${agent_pid:+"$agent_pid"} "$gateway_pid" 2>/dev/null || true; wait ${agent_pid:+"$agent_pid"} "$gateway_pid" 2>/dev/null || true; }
trap cleanup EXIT
key=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
base="http://127.0.0.1:$port"
for ((attempt=0;attempt<50;attempt++)); do
 if curl -fsS "$base/healthz" -o /dev/null 2>/dev/null; then break; fi
 sleep .1
done
"$binary_dir/onprest-agent" --config "$evidence_dir/agent.yaml" > "$evidence_dir/agent.log" 2>&1 &
agent_pid=$!
for ((attempt=0;attempt<100;attempt++)); do
 if curl -fsS -H "Authorization: Bearer $key" "$base/openapi.json" -o "$evidence_dir/openapi.json" 2>/dev/null; then break; fi
 sleep .15
done
[ -s "$evidence_dir/openapi.json" ]
# Doctor succeeds while the running Agent owns its exclusive spool lock.
"$binary_dir/onprest-agent" doctor --config "$evidence_dir/agent.yaml" > "$evidence_dir/doctor.log" 2>&1
printf '%%PDF-1.7\nexample byte snapshot\n' > "$evidence_dir/report.pdf"
curl -fsS -H "Authorization: Bearer $key" --form "document=@$evidence_dir/report.pdf;type=application/pdf" --form 'month="\"2026-10\"";type=application/json' "$base/api/v1/capabilities/save_report" > "$evidence_dir/response.json"
uri=$(python3 - "$evidence_dir/response.json" <<'PY'
import json,sys
print(json.load(open(sys.argv[1]))['result']['files'][0]['uri'])
PY
)
curl -fsS -H "Authorization: Bearer $key" "$uri" -o "$evidence_dir/downloaded.pdf"
cmp "$evidence_dir/report.pdf" "$evidence_dir/exchange/report.pdf"
cmp "$evidence_dir/report.pdf" "$evidence_dir/downloaded.pdf"
curl -fsS -H "Authorization: Bearer $key" -H 'Content-Type: application/json' -H 'MCP-Protocol-Version: 2025-11-25' --data '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"save_report","arguments":{"document":{"name":"inline.pdf","mime_type":"application/pdf","data":"AQ=="}}}}' "$base/mcp" > "$evidence_dir/mcp.json"
python3 - "$evidence_dir" <<'PY'
import base64,hashlib,json,sys
from pathlib import Path
p=Path(sys.argv[1]);v=json.loads((p/'response.json').read_text());d=v['result']['files'][0];b=(p/'report.pdf').read_bytes();assert d['size_bytes']==len(b) and d['sha256']==hashlib.sha256(b).hexdigest();r=json.loads((p/'mcp.json').read_text());assert r['result'].get('isError') is not True;assert (p/'exchange/inline.pdf').read_bytes()==b'\x01';print(json.dumps({'evidence':str(p),'multipart_bytes':len(b),'multipart_sha256':d['sha256'],'MCP_inline_bytes':1,'validate_no_spool':True,'doctor_with_running_spool_lock':True}))
PY
