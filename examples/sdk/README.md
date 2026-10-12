# Go SDK example: customer CSV

The [customer module](customer/main.go) is a small capability written in Go with the [Onprest SDK](../../sdk/go/). It takes a customer name and returns it as JSON together with a one-row CSV file. This example builds the module, tests it locally, and then calls it through the gateway.

You need Go 1.26.9 or later and the `onprest-agent` and `onprest-gateway` binaries on your `PATH`. Run the commands from the root of a clone of this repository: the module builds against the SDK in this checkout. Your own modules require a released `github.com/viewlegacy/onprest/sdk/go` version instead.

## Build

Copy the configuration and build the module to the path its capability expects:

```sh
EXAMPLE=/tmp/onprest-sdk-example
mkdir -p "$EXAMPLE"
cp examples/sdk/agent.yaml examples/sdk/gateway.env examples/sdk/params.json "$EXAMPLE/"
onprest-agent code build --config "$EXAMPLE/agent.yaml" --capability customer_csv \
  --source ./examples/sdk/customer
onprest-agent validate --config "$EXAMPLE/agent.yaml"
```

[agent.yaml](agent.yaml) defines `customer_csv`. Its input pattern allows only letters, digits, and spaces, which keeps spreadsheet formulas out of the CSV. The module builds the CSV in memory, so it runs in the sandbox with no permissions.

## Test locally

Run the capability without the gateway and save the returned file:

```sh
mkdir -p "$EXAMPLE/saved"
onprest-agent code test --config "$EXAMPLE/agent.yaml" --capability customer_csv \
  --params-file "$EXAMPLE/params.json" --output-dir "$EXAMPLE/saved" \
  --format json 2> "$EXAMPLE/diagnostic.log"
```

stdout shows `"data":{"customer":"Alice"}` and the CSV's metadata, including a `saved_path` under `saved/`. The module's log message is in `diagnostic.log`:

```text
... info source=wasm event=wasm_log capability="customer_csv" request_id="..." message="generated customer.csv"
```

After you edit the module, rebuild and test in one step. The test runs only if the build succeeds:

```sh
onprest-agent code test --build --config "$EXAMPLE/agent.yaml" --capability customer_csv \
  --source ./examples/sdk/customer --force --params-file "$EXAMPLE/params.json"
```

## Run and call

The credentials in this example are public; use them only on loopback. Start the gateway:

```sh
set -a
. /tmp/onprest-sdk-example/gateway.env
set +a
onprest-gateway
```

Start the agent in another terminal:

```sh
onprest-agent --config /tmp/onprest-sdk-example/agent.yaml
```

When `http://127.0.0.1:8080/healthz` reports `"agent_connected": true`, call the capability:

```sh
export ONPREST_API_KEY=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"customer":"Alice"}' \
  http://127.0.0.1:8080/api/v1/capabilities/customer_csv
```

The response has `result.data: {"customer":"Alice"}` and one file, `customer.csv`. Download it with the same key, using the `uri` from the response:

```sh
FILE_URI='paste result.files[0].uri here'
curl --fail --show-error -H "Authorization: Bearer $ONPREST_API_KEY" "$FILE_URI"
```

```csv
customer
Alice
```

Over MCP, call the tool `customer_csv` with `arguments: {"customer":"Alice"}`. Stop both binaries with Ctrl+C when you are done.

To deploy, copy `customer.wasm` and the YAML to the agent host; it needs no Go or extra runtime. [Writing code in Go](https://docs.onprest.viewlegacy.com/capabilities/code/go) explains each step in more detail.
