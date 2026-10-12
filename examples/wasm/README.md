# Raw host ABI example

This module talks to the agent through its low-level host interface directly, without the Go SDK. It reads a file from the configuration directory with ordinary WASI file I/O, opens the file uploaded in the current call, and returns data plus a copy of that file. Read it if you want to see what the SDK does underneath; to write your own capabilities, start from the [SDK example](../sdk/) instead.

The capability in [agent.yaml](agent.yaml) runs in the sandbox with a single read grant for the configuration directory. The uploaded file needs no grant because it belongs to the call.

## Build and run

You need Go 1.26.9 or later and the `onprest-agent` and `onprest-gateway` binaries on your `PATH`. From the root of a clone of this repository, build the module with the example's helper and copy the configuration:

```sh
mkdir -p /tmp/onprest-wasm-example
go run ./examples/wasm/build -output /tmp/onprest-wasm-example/module.wasm
cp examples/wasm/agent.yaml examples/wasm/gateway.env examples/wasm/message.txt /tmp/onprest-wasm-example/
onprest-agent validate --config /tmp/onprest-wasm-example/agent.yaml
```

The credentials are public; use them only on loopback. Start the gateway, then the agent in another terminal:

```sh
set -a
. /tmp/onprest-wasm-example/gateway.env
set +a
onprest-gateway
```

```sh
onprest-agent --config /tmp/onprest-wasm-example/agent.yaml
```

When `http://127.0.0.1:8080/healthz` reports `"agent_connected": true`, call `wasm_copy` with an inline file:

```sh
export ONPREST_API_KEY=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"world","attachment":{"name":"input.bin","mime_type":"application/octet-stream","data":"AP8BAg=="}}' \
  http://127.0.0.1:8080/api/v1/capabilities/wasm_copy
```

The response has `result.data: {"greeting":"Hello, world","input_bytes":4}` and one file, `copy.bin`. The module also returns a private field that the output declaration drops. Download the file with the `uri` from the response:

```sh
FILE_URI='paste result.files[0].uri here'
curl --fail --show-error -H "Authorization: Bearer $ONPREST_API_KEY" "$FILE_URI" --output copy.bin
```

`copy.bin` holds the four uploaded bytes `00 ff 01 02`. Stop both binaries with Ctrl+C when you are done.
