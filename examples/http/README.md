# Call a fixed on-prem HTTP API

Use `onprest-gateway` and `onprest-agent` as separate binaries to bind validated arguments to an existing API and publish selected JSON, without a database. This example uses public demonstration signing and API credentials on loopback. Generate deployment credentials with the [Provisioning CLI](https://docs.onprest.viewlegacy.com/reference/cli).

Save [agent.yaml](agent.yaml) and [gateway.env](gateway.env) beside `onprest-agent` and `onprest-gateway`. For a local demonstration, also save [upstream.py](upstream.py) and run it in its own shell:

```sh
python3 upstream.py
```

This optional fixture needs Python only on your demonstration machine; an Agent connecting to an existing API requires no Python or additional runtime. It listens on `127.0.0.1:8090` and serves `POST /stock`.

Validate the configuration, then start the Gateway in one shell:

```sh
./onprest-agent validate --config ./agent.yaml
set -a
. ./gateway.env
set +a
./onprest-gateway
```

Validation prepares the source without sending business HTTP requests. Start the Agent in another shell:

```sh
./onprest-agent --config ./agent.yaml
```

The Agent user needs permission to create private `identity` and `spool` directories beside the configuration. In another shell, check the connection:

```sh
curl --fail --show-error http://127.0.0.1:8080/healthz
```

Wait until the response reports `"agent_connected": true`, then call:

```sh
export ONPREST_API_KEY=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"item":"widget-01"}' \
  http://127.0.0.1:8080/api/v1/capabilities/get_inventory
```

The response contains `result.data: {"item":"widget-01","available":7}` and `result.files: []`. The Agent sends the fixed channel alongside the input item and excludes the upstream `internal_note` from the public result. MCP calls the same `get_inventory` capability with `arguments: {"item":"widget-01"}`.

Use an HTTPS origin and narrowly permitted methods/CIDRs for deployment. Keep endpoint paths and credentials in Agent configuration. HTTP effects cannot generally be rolled back: a timeout or unknown outcome is not permission to retry. See the [HTTP executor reference](https://docs.onprest.viewlegacy.com/agent/capability-yaml#http-executors) for other body forms, file input, redirect policy, and limits.
