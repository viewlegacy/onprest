# CSV example: filter rows and download a new CSV

Two capabilities read the same sales data and return only the rows for one month, without a database:

- `summarize_sales` reads [sales.csv](sales.csv) from a folder and returns the selected rows as a CSV download.
- `convert_upload` does the same with CSV files uploaded in the call, and returns both JSON rows and the CSV.

Both publish only `id` and `amount`. The file's `month` column is used for filtering, and its `internal_note` column is never returned.

## Run it

Put [agent.yaml](agent.yaml), [gateway.env](gateway.env), and [sales.csv](sales.csv) in a directory next to the `onprest-agent` and `onprest-gateway` binaries. Place the CSV in the `reports` folder the way a producer should: write it under a temporary name, then rename it.

```sh
mkdir -p reports
cp sales.csv reports/sales.tmp
mv reports/sales.tmp reports/sales.csv
./onprest-agent validate --config ./agent.yaml
```

The credentials are public; use them only on loopback. Start the gateway, then the agent in another shell:

```sh
set -a
. ./gateway.env
set +a
./onprest-gateway
```

```sh
./onprest-agent --config ./agent.yaml
```

## Download from the folder

When `http://127.0.0.1:8080/healthz` reports `"agent_connected": true`:

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

```csv
id,amount
00123,42
```

The response body is the file itself, because the capability uses `delivery.rest: direct`. `amount` is parsed as an integer, while `id` stays a string and keeps its leading zeros.

## Convert an upload

Send the same file in the call:

```sh
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -F 'month="\"2026-10\"";type=application/json' \
  -F 'documents=@./sales.csv;type=text/csv' \
  http://127.0.0.1:8080/api/v1/capabilities/convert_upload
```

The response has `result.data.rows: [{"id":"00123","amount":42}]`, `count: 1`, and a link to the generated CSV. Download it and compare:

```sh
CSV_URI='paste result.files[0].uri here'
curl --fail --show-error -H "Authorization: Bearer $ONPREST_API_KEY" "$CSV_URI" --output uploaded-sales.csv
cmp selected-sales.csv uploaded-sales.csv
```

`cmp` prints nothing: both paths produced the same file. To send several files, repeat the `documents` part; they must share the same header. Uploaded files are not saved into `reports`. Over MCP, `convert_upload` takes the file inline in `arguments` and returns the CSV embedded in the tool result.

See [CSV](https://docs.onprest.viewlegacy.com/capabilities/csv) for parsing and filtering options, and [API: Files](https://docs.onprest.viewlegacy.com/api/files) for the upload and download forms.
