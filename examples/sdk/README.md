# Go SDK customer CSV example

The [customer module](customer/main.go) uses `onprest.Run` to return customer JSON and a CSV file in one call. This example builds it, runs it through the two normal binaries, and downloads the CSV. See the [SDK reference](../../sdk/go/README.md) for the public API and reader ownership rules.

Make matching `onprest-agent` and `onprest-gateway` binaries available on `PATH`. The build steps require Go 1.26.9 or later on the author machine. Run them from the OSS repository root. The example's local module replacement builds this checkout; application modules can pin `github.com/viewlegacy/onprest/sdk/go` independently.

## Build and configure

Compile the module and append metadata describing its actual imports:

```sh
mkdir -p /tmp/onprest-sdk-example
go run ./examples/sdk/build --output /tmp/onprest-sdk-example/customer.wasm
```

Reuse the loopback connection settings and demonstration credentials from the [Wasm example](../wasm/README.md), replacing its capability with `customer_csv`:

```sh
sed '/^capabilities:/,$d' examples/wasm/agent.yaml > /tmp/onprest-sdk-example/agent.yaml
sed 's/wasm_copy/customer_csv/g' examples/wasm/gateway.env > /tmp/onprest-sdk-example/gateway.env
cat >> /tmp/onprest-sdk-example/agent.yaml <<'YAML'
capabilities:
  customer_csv:
    executor:
      type: wasm
      module: ./customer.wasm
      effects: none
      sandbox: {enabled: true}
    input:
      properties:
        customer: {type: string, pattern: '^[A-Za-z0-9 ]{1,64}$'}
      required: [customer]
    output: {data: {}, files: {}}
YAML
onprest-agent validate --config /tmp/onprest-sdk-example/agent.yaml
```

The input pattern excludes spreadsheet formula prefixes. The module creates the CSV in memory, so sandbox deny-all needs no filesystem or network grants. The Agent user needs permission to create private `identity` and `spool` directories beside the configuration.

## Run and call

Use the public demonstration credentials only on loopback; replace them for deployment. Start the Gateway in one terminal:

```sh
set -a
. /tmp/onprest-sdk-example/gateway.env
set +a
onprest-gateway
```

Start the Agent in another terminal:

```sh
onprest-agent --config /tmp/onprest-sdk-example/agent.yaml
```

Wait until `http://127.0.0.1:8080/healthz` reports `"agent_connected": true`, then call:

```sh
export ONPREST_API_KEY=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"customer":"Alice"}' \
  http://127.0.0.1:8080/api/v1/capabilities/customer_csv
```

The response contains `result.data: {"customer":"Alice"}` and a file descriptor named `customer.csv`. Copy `result.files[0].uri` into `FILE_URI` and download it with the same credential:

```sh
FILE_URI='paste the returned URI here'
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  "$FILE_URI" --output /tmp/onprest-sdk-example/customer.csv
cat /tmp/onprest-sdk-example/customer.csv
```

The file contains:

```csv
customer
Alice
```

MCP `tools/call` uses `name: customer_csv` and `arguments: {"customer":"Alice"}`. The default delivery is a reference; see the [file API documentation](https://docs.onprest.viewlegacy.com/api/files) for retrieval and lifetime rules. Stop both binaries with Ctrl+C when finished.

Deploy the completed module and configuration with the Agent binary. The Agent includes the runtime and needs no compiler. The build helper is example tooling for an author's machine and adds no deployment dependency.
