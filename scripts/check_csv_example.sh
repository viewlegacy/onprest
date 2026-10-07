#!/usr/bin/env bash
set -euo pipefail
# Development verification of the published two-binary example. No DB needed.
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
binary_dir=${BIN_DIR:-"$repo_dir/dist"}
evidence_dir=${P11_BINARY_EVIDENCE_DIR:-$(mktemp -d /tmp/onprest-csv.XXXXXX)}
mkdir -p "$evidence_dir/reports"
cp "$repo_dir/examples/csv/agent.yaml" "$evidence_dir/agent.yaml"
cp "$repo_dir/examples/csv/gateway.env" "$evidence_dir/gateway.env"
port=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
python3 - "$evidence_dir/agent.yaml" "$port" <<'PY'
from pathlib import Path
import sys
p=Path(sys.argv[1]);p.write_text(p.read_text().replace('127.0.0.1:8080','127.0.0.1:'+sys.argv[2]))
PY
"$binary_dir/onprest-agent" validate --config "$evidence_dir/agent.yaml" > "$evidence_dir/validate.log" 2>&1
[ ! -e "$evidence_dir/spool" ]
[ -z "$(ls -A "$evidence_dir/reports")" ]
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
cp "$repo_dir/examples/csv/sales.csv" "$evidence_dir/reports/sales.tmp"
mv "$evidence_dir/reports/sales.tmp" "$evidence_dir/reports/sales.csv"
curl -fsS -H "Authorization: Bearer $key" -H 'Content-Type: application/json' --data '{"month":"2026-10"}' "$base/api/v1/capabilities/summarize_sales" -o "$evidence_dir/folder.csv"
curl -fsS -H "Authorization: Bearer $key" --form 'month="\"2026-10\"";type=application/json' --form "documents=@$evidence_dir/reports/sales.csv;type=text/csv" "$base/api/v1/capabilities/convert_upload" > "$evidence_dir/upload.json"
uri=$(python3 - "$evidence_dir/upload.json" <<'PYJSON'
import json,sys
print(json.load(open(sys.argv[1]))['result']['files'][0]['uri'])
PYJSON
)
curl -fsS -H "Authorization: Bearer $key" "$uri" -o "$evidence_dir/upload.csv"
python3 - "$evidence_dir" <<'PYJSON'
from pathlib import Path
import base64,json,sys
p=Path(sys.argv[1]);params={'month':'2026-10','documents':[{'name':'sales.csv','mime_type':'text/csv','data':base64.b64encode((p/'reports/sales.csv').read_bytes()).decode()}]}
(p/'request.json').write_text(json.dumps({'jsonrpc':'2.0','id':1,'method':'tools/call','params':{'name':'convert_upload','arguments':params}}))
PYJSON
curl -fsS -H "Authorization: Bearer $key" -H 'Content-Type: application/json' -H 'MCP-Protocol-Version: 2025-11-25' --data-binary "@$evidence_dir/request.json" "$base/mcp" > "$evidence_dir/mcp.json"
python3 - "$evidence_dir" <<'PYJSON'
import base64,hashlib,json,sys
from pathlib import Path
p=Path(sys.argv[1]);expected=b'id,amount\r\n00123,42\r\n';assert (p/'folder.csv').read_bytes()==expected;assert (p/'upload.csv').read_bytes()==expected
r=json.loads((p/'upload.json').read_text())['result'];assert r['data']=={'rows':[{'id':'00123','amount':42}],'count':1};d=r['files'][0];assert d['size_bytes']==len(expected) and d['sha256']==hashlib.sha256(expected).hexdigest()
rpc=json.loads((p/'mcp.json').read_text())['result'];assert rpc.get('isError') is not True;assert rpc['structuredContent']['result']['data']==r['data'];resource=rpc['content'][1]['resource'];actual=resource['text'].encode() if 'text' in resource else base64.b64decode(resource['blob']);assert actual==expected
assert sorted(v.name for v in (p/'reports').iterdir())==['sales.csv'];print(json.dumps({'evidence':str(p),'folder_upload_MCP_bytes':len(expected),'sha256':d['sha256'],'validate_no_spool':True,'doctor_with_running_spool_lock':True,'upload_saved_to_source':False}))
PYJSON
