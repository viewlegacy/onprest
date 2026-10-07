# Filter CSV and generate a download

Use the two released binaries to filter a folder CSV or uploaded CSV files, without a database. These signing and API credentials are public demonstration values: run on loopback only. Generate deployment credentials with the [Provisioning CLI](https://docs.onprest.viewlegacy.com/reference/cli).

Save [agent.yaml](agent.yaml), [gateway.env](gateway.env) and [sales.csv](sales.csv) in a working directory beside `onprest-agent` and `onprest-gateway`. Prepare the source, then validate:

```sh
mkdir -p reports
cp sales.csv reports/sales.tmp
mv reports/sales.tmp reports/sales.csv
./onprest-agent validate --config ./agent.yaml
```

Start Gateway in one shell:

```sh
set -a
. ./gateway.env
set +a
./onprest-gateway
```

Start Agent in another shell:

```sh
./onprest-agent --config ./agent.yaml
```

The Agent user needs permission to create private `identity` and `spool` directories beside this configuration. Wait until `/healthz` reports `"agent_connected": true`, then download the selected rows:

```sh
export ONPREST_API_KEY=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"month":"2026-10"}' \
  http://127.0.0.1:8080/api/v1/capabilities/summarize_sales \
  --output selected-sales.csv
cat selected-sales.csv
```

The generated file has `id,amount` followed by `00123,42`. `amount` is parsed as an integer; undeclared parser columns remain strings, so `00123` keeps its leading zeroes. Filtering uses `month`, while the shared output definition excludes `month` and `internal_note` from the generated CSV.

Send the same file as a one-call multipart input to receive JSON plus a generated-file reference:

```sh
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -F 'month="\"2026-10\"";type=application/json' \
  -F 'documents=@./sales.csv;type=text/csv' \
  http://127.0.0.1:8080/api/v1/capabilities/convert_upload
```

The response contains `result.data.rows: [{"id":"00123","amount":42}]`, `count: 1`, and one generated CSV descriptor. Copy `result.files[0].uri` into `CSV_URI`, then download it with the same Authorization header:

```sh
CSV_URI='paste the returned URI here'
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  "$CSV_URI" --output uploaded-sales.csv
cmp selected-sales.csv uploaded-sales.csv
```

The comparison succeeds with no output: both paths generated the same selected CSV. Repeat the `documents` part for multiple files with the same header names and order; rows follow input order. Uploaded originals are not saved into `reports`. MCP `convert_upload` accepts inline file arguments and returns the generated CSV as an inline resource.

Publish folder inputs through a temporary file and atomic rename, as above. See the [CSV executor reference](https://docs.onprest.viewlegacy.com/agent/capability-yaml#csv-executors) for parsing, filtering and limits, and [File Input and Output](https://docs.onprest.viewlegacy.com/api/files) for request forms and delivery.
