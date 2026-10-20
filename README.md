# Onprest

Onprest lets SaaS products, partner systems, internal tools, and AI agents call selected operations inside your on-premises environment through REST and MCP, without opening inbound firewall ports, copying data to the cloud, or letting credentials leave the network.

> Keep the legacy system. Modernize the access layer.
> Expose capabilities, not your on-prem.

Most integrations with on-prem systems start from the wrong primitive: access to the system itself.

Onprest starts from a smaller primitive: a named business operation. Callers invoke `get_customer`, `download_invoice`, or `check_inventory`. They never receive a DSN, a file share, an internal URL, or a schema-wide CRUD surface. What each operation actually does (the SQL, the folder, the upstream API, the code) is defined on-prem in `capability.yaml` and stays there.

MCP is a first-class surface, not an afterthought: AI agents call named business operations, never raw SQL or internal endpoints.

## Why Onprest exists

On-prem systems often need to be reachable from modern SaaS, partners, internal tools, and AI clients. The usual options create more exposure than conservative environments can accept:

- opening inbound firewall paths into the customer network
- exposing SQL-like interfaces through a proxy
- copying business data into a cloud database
- tunnelling file shares or internal APIs out of the network
- building and maintaining one-off APIs beside each legacy system
- giving a cloud iPaaS or integration platform broad access to databases and file servers

Onprest takes a narrower position:

> Keep the legacy system in place. Expose only approved business operations.

It is built for teams that cannot rewrite, migrate, or move their legacy systems, but still need selected parts of them to become safely available to modern software and AI agents.

## What a capability can run

| Executor | What it does | Try it |
|---|---|---|
| `db` | Runs one reviewed SQL statement on PostgreSQL, MySQL, SQL Server, or Oracle | [Quick Start](#quick-start) |
| `csv` | Reads CSV files from a folder or an upload, filters rows, and returns JSON or a generated CSV | [CSV example](examples/csv/) |
| `file` | Returns completed files from a folder, or saves uploaded files into one | [Download](examples/file-output/), [upload](examples/file-input/) |
| `http` | Calls a fixed internal HTTP API with validated arguments | [HTTP example](examples/http/) |
| `wasm` | Runs a WebAssembly module you wrote, for example in Go with the [SDK](sdk/go/) | [SDK example](examples/sdk/) |

All of them share the same input validation, execution policy, and output selection, and callers use them the same way through REST and MCP.

## What Onprest is

- a narrow capability tunnel
- for approved business operations
- defined on-prem
- callable through REST and MCP
- with no inbound firewall dependency
- with no SQL, credentials, or internal endpoints stored in the gateway
- with no cloud replica of your business data

## What Onprest is not

- a SQL-over-HTTP proxy
- a database or file-share browser
- a schema-wide CRUD generator
- a generic reverse proxy for internal APIs
- an ETL pipeline or a cloud replica of your data
- a general-purpose iPaaS replacement
- a way to give AI agents broad access to your systems

If your goal is instant CRUD over an entire database, a database API generator may be enough.

## How it works

The OSS core is two binaries and one agent-side file:

- `onprest-gateway`: the public REST, MCP, and OpenAPI edge
- `onprest-agent`: runs on-prem, connects out to the gateway, and owns `capability.yaml`

```text
API client / AI agent
        |
        | REST / MCP / OpenAPI
        v
onprest-gateway
        ^
        | WebSocket, outbound from on-prem
        |
onprest-agent + capability.yaml
        |
        v
database / folder / internal API / Wasm module
```

The gateway knows who may call which capability. The agent knows what each capability means, validates the input, runs it, and decides which fields and files may leave the network.

| Concern | Gateway | Agent |
|---|:---:|:---:|
| Public REST / MCP / OpenAPI endpoint | Yes | No |
| API key authentication | Yes | No |
| Capability authorization | Yes | Yes |
| Rate limiting | Per source IP | Per capability |
| SQL, folder paths, upstream URLs, code | No | Yes |
| Database and API credentials | No | Yes |
| Input validation and execution policy | Ingress check | Yes, before execution |
| Output selection | No | Yes |
| Detailed errors | No | Local log only |
| Agent private key | No | Yes |

This split is the core of Onprest. The gateway is public and operationally useful, but it does not hold the sensitive parts of the integration. Onprest assumes the gateway may be observed or compromised:

- compromise the gateway: you get no SQL, credentials, or internal endpoints, and you cannot add operations
- compromise an API key: you only get the capabilities assigned to that key
- ask for an unknown operation: the agent rejects it
- return more data than intended: only the declared output leaves the agent
- trigger an error: the detail stays in the agent's local log

Both binaries are static Go builds. The agent host needs no Docker, Go toolchain, or database client libraries (Oracle included).

## Define a capability

A capability is a named operation in `capability.yaml`:

```yaml
capabilities:
  get_customer:
    description: Fetch one customer by id.
    executor:
      type: db
      source: primary
      sql: select id, name, email from customers where id = :customer_id
    input:
      properties:
        customer_id: {type: integer, minimum: 1}
      required: [customer_id]
    policy:
      timeout: 3s
      max_rows: 1
    output:
      data: {}
      items:
        properties:
          id: {type: integer}
          name: {type: string}
          email: {type: string}
```

`input` is what callers may send, `policy` bounds the execution, and `output` decides what leaves the agent. The same shape fronts an internal API:

```yaml
  check_inventory:
    executor:
      type: http
      source: inventory
      method: POST
      path: /stock
      body: {kind: json, fields: {item: {input: /item}}}
      response: {kind: json}
    input: {properties: {item: {type: string}}, required: [item]}
    output: {data: {}, properties: {item: {type: string}, available: {type: integer}}}
```

The agent validates the whole file at startup and prepares every source it references before it connects. See [Capabilities](https://docs.onprest.viewlegacy.com/capabilities) for every executor.

## AI agents and MCP

AI agents should not need raw database access to answer business questions. With Onprest, an AI agent receives a tool list generated from the capabilities its API key may call. Each tool has an input schema, execution limits, and an output contract defined on-prem.

Instead of a DSN, a file share, or a SQL executor, you give the agent narrow tools such as `get_customer`, `search_orders`, `check_inventory`, or `download_invoice`. The same capabilities are available through REST and OpenAPI for ordinary applications.

## Security at a glance

- The agent connects outbound; no inbound path into the on-prem network is required.
- The gateway stores no SQL, source credentials, or agent private key. It holds the agent public key and bcrypt-hashed API keys.
- Agents authenticate with Ed25519 signatures, and only one agent connection is accepted at a time.
- API keys are scoped to named capabilities; OpenAPI and MCP tool lists are filtered per key.
- The agent validates every input before running anything and publishes only declared output.
- Gateway logs contain no request params or error detail; that detail stays in the agent's local log.

Onprest's policies are an additional control, not a replacement for least privilege. Give the agent a read-only database user when its capabilities only read. Code capabilities run with the agent's OS permissions unless you turn the sandbox on; turn it on and grant only what each module needs. See the [security model](https://docs.onprest.viewlegacy.com/security).

## Quick Start

You need Linux or macOS and Docker with Compose. Docker only runs the disposable example PostgreSQL database; Onprest itself needs no Docker, Go toolchain, or source checkout. Windows steps are in the [full Quick Start](https://docs.onprest.viewlegacy.com/quick-start).

Download the binary archive for your host and the Quick Start files from the latest release:

| Host | Download |
|---|---|
| Linux x64 | [⬇ Binary archive](https://github.com/viewlegacy/onprest/releases/latest/download/onprest-linux-amd64.tar.gz) |
| Linux ARM64 | [⬇ Binary archive](https://github.com/viewlegacy/onprest/releases/latest/download/onprest-linux-arm64.tar.gz) |
| macOS Intel | [⬇ Binary archive](https://github.com/viewlegacy/onprest/releases/latest/download/onprest-darwin-amd64.tar.gz) |
| macOS Apple silicon | [⬇ Binary archive](https://github.com/viewlegacy/onprest/releases/latest/download/onprest-darwin-arm64.tar.gz) |
| Windows x64 | [⬇ Binary archive](https://github.com/viewlegacy/onprest/releases/latest/download/onprest-windows-amd64.zip) |
| All hosts | [⬇ Quick Start files](https://github.com/viewlegacy/onprest/releases/latest/download/onprest-quickstart.tar.gz) |

Extract both into a working directory (replace `linux-amd64` with your host) and run the remaining commands from there:

```sh
mkdir -p onprest/examples
cd onprest
tar -xzf ../onprest-linux-amd64.tar.gz --strip-components=1
tar -xzf ../onprest-quickstart.tar.gz --strip-components=1 -C examples
```

Start the example database and check the agent configuration:

```sh
docker compose -f examples/postgres.compose.yml up -d --wait
./onprest-agent validate --config examples/capability.postgres.yaml
```

Start the gateway, then the agent in a second shell:

```sh
set -a; . examples/gateway.env; set +a
./onprest-gateway
```

```sh
./onprest-agent --config examples/capability.postgres.yaml
```

In a third shell, wait until `curl -sS http://localhost:8080/healthz` reports `"agent_connected": true`, then call the capability with the example API key:

```sh
export ONPREST_API_KEY='orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM'
curl -sS \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"customer_id":1}' \
  http://localhost:8080/api/v1/capabilities/get_customer
```

```json
{
  "request_id": "01J5ABCDEF1234567890ABCDEF01",
  "result": {
    "data": {
      "rows": [{ "id": 1, "name": "Ada Lovelace", "email": "ada@example.com" }],
      "count": 1
    },
    "files": []
  }
}
```

The same capability is an MCP tool:

```sh
curl -sS \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -H "MCP-Protocol-Version: 2025-11-25" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{"customer_id":1}}}' \
  http://localhost:8080/mcp
```

Stop both binaries with Ctrl+C and remove the database with `docker compose -f examples/postgres.compose.yml down -v`. The example keys and password are public; never reuse them. The [full Quick Start](https://docs.onprest.viewlegacy.com/quick-start) continues with a mutation and OpenAPI, and the examples above run the other executors without a database.

## Build from source

Building from source is optional. With the Go version declared in `go.mod`:

```sh
make build        # dist/onprest-gateway and dist/onprest-agent
make build-cross  # every supported OS/CPU target
```

## Documentation

The full documentation is at [docs.onprest.viewlegacy.com](https://docs.onprest.viewlegacy.com), with its source under [`docs/`](docs/). Good next reads:

- [How it works](https://docs.onprest.viewlegacy.com/architecture)
- [Capabilities](https://docs.onprest.viewlegacy.com/capabilities)
- [REST API](https://docs.onprest.viewlegacy.com/api/rest) and [MCP](https://docs.onprest.viewlegacy.com/api/mcp)
- [Security](https://docs.onprest.viewlegacy.com/security)
- [Deployment](https://docs.onprest.viewlegacy.com/operations/deployment)

## Repository boundary

This repository contains only the Apache-2.0 OSS core: `onprest-gateway`, `onprest-agent`, and the Go SDK for code capabilities. Managed products, dashboards, and their operating policies are outside this repository and are not OSS core dependencies.

## License

Apache-2.0. See [LICENSE](LICENSE).
