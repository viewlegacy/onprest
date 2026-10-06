# File input example

Use the two released binaries with this configuration and an existing writable `exchange` folder. The included signing/API credentials are public loopback demonstration values. Generate your own credentials for deployment. File inputs use one capability call and the existing Agent outbound connection.

Place `onprest-agent`, `onprest-gateway`, `agent.yaml`, and `gateway.env` in one working directory. Create the source and validate without saving a business file:

```sh
mkdir -p exchange
./onprest-agent validate --config ./agent.yaml
```

Start Gateway in one shell:

```sh
set -a
. ./gateway.env
set +a
./onprest-gateway
```

Start Agent in another shell with `./onprest-agent --config ./agent.yaml`. The Agent user needs private writable identity/spool directories beside this configuration, and permission to save in `exchange`. Wait for `/healthz` to report `agent_connected: true` and for authenticated `/openapi.json` to become available.

Prepare your PDF as `report.pdf`, then call with a binary file and a JSON string argument:

```sh
export ONPREST_API_KEY=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  --form 'document=@./report.pdf;type=application/pdf' \
  --form 'month="\"2026-10\"";type=application/json' \
  http://127.0.0.1:8080/api/v1/capabilities/save_report
```

The accepted file is saved as `exchange/report.pdf`. The response has null data and one independent file descriptor. Copy its `uri` into the GET command below and use the same credential:

```sh
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  'http://127.0.0.1:8080/api/v1/files/RETURNED_FILE_ID' \
  --output returned-report.pdf
cmp ./report.pdf ./exchange/report.pdf
cmp ./report.pdf ./returned-report.pdf
```

Both comparisons exit successfully with no output when the bytes match. Repeating the same filename conflicts by default; choose a new input filename for another business call. Outputs use independent delivery/lifetime settings.

REST JSON and MCP can instead send `document: {name, mime_type, data}` with canonical padded Base64. Their encoded request limit is 1 MiB by default. Larger inputs use multipart. See [File input](https://docs.onprest.viewlegacy.com/api/files#file-input) for naming, schema, limits, deadlines and failure contracts. TLS and reverse proxy choice belong to the deployment environment.
