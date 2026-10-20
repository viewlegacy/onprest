# HTTP example: call an internal API

The `get_inventory` capability turns one endpoint of an internal inventory API into a named operation. The agent sends the caller's `item` together with a fixed `channel` value to `POST /stock`, and publishes only `item` and `available` from the response. No database is needed.

## Run it

Put [agent.yaml](agent.yaml) and [gateway.env](gateway.env) in a directory next to the `onprest-agent` and `onprest-gateway` binaries. To stand in for the internal API, also save [upstream.py](upstream.py) and run it in its own shell. It listens on `127.0.0.1:8090`; Python is needed only for this demonstration.

```sh
python3 upstream.py
```

Validate the configuration. This prepares the API source without sending it a request:

```sh
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

## Call it

When `http://127.0.0.1:8080/healthz` reports `"agent_connected": true`:

```sh
export ONPREST_API_KEY=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"item":"widget-01"}' \
  http://127.0.0.1:8080/api/v1/capabilities/get_inventory
```

The response has `result.data: {"item":"widget-01","available":7}`. The upstream API also returns an `internal_note`, which the output declaration drops. Over MCP, call the tool `get_inventory` with `arguments: {"item":"widget-01"}`.

For a real API, use an `https://` base URL, allow only the methods you need, and keep the token in `credential_file`. The agent never retries a request: if a call times out after it was sent, check the upstream system before trying again. See [HTTP APIs](https://docs.onprest.viewlegacy.com/capabilities/http) for query parameters, form and multipart bodies, file responses, and redirects.
