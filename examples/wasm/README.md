# Wasm host ABI example

This module uses the current internal host ABI directly, reads a config-relative file through standard Go WASI file I/O, opens the current request's uploaded file, and returns data plus an independent file snapshot. The output contract removes its private data field. It needs only a read grant for the configuration directory; InputFile handles are request-owned and need no FS grant.

Make the matching `onprest-agent` and `onprest-gateway` binaries available on `PATH`. On an author machine with Go, run from the OSS repository root:

```sh
go run ./examples/wasm/build -output /tmp/onprest-example.wasm
mkdir -p /tmp/onprest-wasm-example
cp examples/wasm/agent.yaml examples/wasm/gateway.env examples/wasm/message.txt /tmp/onprest-wasm-example/
cp /tmp/onprest-example.wasm /tmp/onprest-wasm-example/module.wasm
onprest-agent validate --config /tmp/onprest-wasm-example/agent.yaml
```

Deploy the completed module and configuration with the two normal binaries. No Go compiler or extra Wasm runtime is needed on the Agent installation. Use the public demonstration credentials only on loopback; replace them for deployment.

Start the Gateway in one terminal:

```sh
set -a
. /tmp/onprest-wasm-example/gateway.env
set +a
onprest-gateway
```

Start the Agent in another terminal:

```sh
onprest-agent --config /tmp/onprest-wasm-example/agent.yaml
```

The Agent user needs permission to create private `identity` and `spool` directories beside this configuration. Wait until `http://127.0.0.1:8080/healthz` reports `"agent_connected": true`, then call `wasm_copy`:

```sh
export ONPREST_API_KEY=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"world","attachment":{"name":"input.bin","mime_type":"application/octet-stream","data":"AP8BAg=="}}' \
  http://127.0.0.1:8080/api/v1/capabilities/wasm_copy
```

The response contains `result.data: {"greeting":"Hello, world","input_bytes":4}` and one file descriptor for `copy.bin`; the private field is absent. Copy `result.files[0].uri` into `FILE_URI`, then retrieve the independent snapshot with the same credential:

```sh
FILE_URI='paste the returned URI here'
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  "$FILE_URI" --output copy.bin
```

The downloaded file contains the four uploaded bytes (`00 ff 01 02` in hexadecimal). MCP `tools/call` uses `name: wasm_copy` and the same arguments. See the [file API documentation](https://docs.onprest.viewlegacy.com/api/files) for delivery and lifetime rules. Stop both binaries with Ctrl+C when finished.

The helper only builds this internal ABI example and attaches metadata describing its actual imports. For application packages, use the [Agent author CLI](../../docs/app/reference/cli/page.mdx#build-and-test-wasm-code). The Agent's `PWD` is fixed to the configuration base in both sandbox modes, so `message.txt` resolves there independently of the process working directory.
