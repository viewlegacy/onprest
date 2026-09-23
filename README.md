# Onprest

Onprest is an **agent-defined capability tunnel** for legacy databases.

It exposes selected on-prem-defined business capabilities through REST and MCP, without exposing SQL, database credentials, raw schema access, cloud data replicas, or inbound firewall ports.

> Keep the legacy system. Modernize the access layer.  
> Expose capabilities, not your database.

Most AI/database integrations start from the wrong primitive: database access.

Onprest starts from a smaller primitive: a named business capability. AI agents, SaaS products, internal tools, and partner systems call explicit operations such as `get_customer`, `search_orders`, or `check_inventory`. They never receive a DSN, raw SQL access, or a schema-wide CRUD surface.

The public gateway handles routing, identity, edge rate limits, and observability. The on-prem agent owns SQL, credentials, validation, per-capability execution policy, SELECT output filtering, the DML count-only contract, and business meaning. `capability.yaml` defines the only operations that can exist.

MCP is a first-class surface, not an afterthought: AI agents call named business operations, never raw SQL.

## Start Here

- **Try it locally:** [Quick Start](#quick-start)
- **Understand the architecture:** [Architecture](#architecture), [Responsibility Split](#responsibility-split), [Trust Boundary](#trust-boundary)
- **Define capabilities:** [Capability Boundary](#capability-boundary)
- **Use with AI agents:** [AI Agents and MCP](#ai-agents-and-mcp)
- **Review security:** [Security Model at a Glance](#security-model-at-a-glance)
- **Read the docs:** [Documentation](#documentation)

## Why Onprest Exists

Legacy databases often need to be reachable from modern systems, partners, internal tools, or AI/MCP clients. The usual options often create too much exposure for conservative on-prem environments:

- opening inbound firewall paths into the customer network
- exposing SQL-like interfaces through a proxy
- copying business data into a cloud database
- generating schema-wide CRUD APIs over legacy tables
- building and maintaining one-off APIs beside each legacy system
- giving a cloud iPaaS or integration platform broad database access

Onprest takes a narrower position:

> Keep the legacy system in place. Expose only approved business operations.

It is built for teams that cannot rewrite, migrate, or move legacy systems, but still need selected parts of those systems to become safely available to modern SaaS, internal tools, partners, and AI agents.

## What Onprest Is

Onprest is:

- a narrow capability tunnel
- for approved business operations
- defined on-prem
- callable through REST and MCP
- with no inbound firewall dependency
- with no SQL or DB credentials stored in the gateway
- with no cloud replica of your business data

If your goal is to expose a small number of safe, reviewed, business-level operations from a legacy database to SaaS or AI clients, Onprest is designed for that.

## What Onprest Is Not

Onprest is not:

- a SQL-over-HTTP proxy
- a database browser
- a schema-wide CRUD generator
- an ETL pipeline
- a cloud replica of your on-prem database
- a general-purpose iPaaS replacement
- a way to give AI agents broad database access

If your goal is instant CRUD over an entire database, a database API generator may be enough.

## Architecture

The OSS core is intentionally small: two binaries and one agent-side capability file.

- `onprest-gateway`: public REST/MCP/WebSocket edge
- `onprest-agent`: on-prem outbound connector that owns `capability.yaml`

Managed dashboards and operations are available separately; this repository contains the OSS gateway and agent.

```text
API user / MCP client
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
legacy database
```

The gateway knows routing, identity, edge rate limits, and which API keys may call which capability names.

The agent knows what each capability means, how parameters are validated, which prepared SQL is executed, and—for SELECT—which result fields are allowed to leave the customer environment. Mutations return affected count only.

## Responsibility Split

The gateway can be useful without being trusted with meaning.

| Concern | Gateway | Agent |
|---|---:|---:|
| Public REST/MCP endpoint | Yes | No |
| WebSocket edge | Yes | Outbound client |
| API key authentication | Yes | No |
| Capability authorization | Yes | Yes |
| Rate limiting | Yes (per source IP) | Yes (per capability) |
| Request observability | Yes | No |
| OpenAPI/MCP filtering by API key | Yes | No |
| SQL text | No | Yes |
| Database credentials | No | Yes |
| Raw database schema knowledge | No | Yes |
| Business logic and execution meaning | No | Yes |
| Parameter validation | No | Yes |
| Execution policy | No | Yes |
| Prepared SQL execution | No | Yes |
| SELECT output allow-list / mutation count-only contract | No | Yes |
| Detailed DB errors | No | Local only |
| Agent private key | No | Yes |
| Agent public key | Yes | No |

This split is the core of Onprest. The gateway is public and operationally useful, but intentionally does not hold the most sensitive parts of the integration.

## Trust Boundary

Onprest assumes the public gateway may be observed or compromised.

That is why the gateway never stores SQL, DSNs, database credentials, agent private keys, raw schema knowledge, or capability execution rules. It can authenticate callers, apply edge rate limits, check whether an API key may call a capability name, serve the agent-defined public contract, and forward the request to the connected agent.

The on-prem agent is the trust boundary. It validates inputs, applies execution policy, executes prepared SQL, filters SELECT output or returns DML affected count, and keeps detailed DB errors local.

In short:

- compromise the gateway: you do not get SQL or database credentials
- compromise an API key: you only get the capabilities assigned to that key
- ask for an unknown operation: the agent rejects it
- return extra columns from SELECT: `result` filters them before they leave the agent; DML cannot define `result` and returns count only
- trigger a DB error: detailed DB error information stays in the agent-local log

## AI Agents and MCP

AI agents should not need raw database access to answer business questions.

With Onprest, an AI agent receives a list of tools generated from approved capabilities. Each tool has a parameter contract, execution policy, and output contract defined on-prem.

Instead of giving an AI agent a DSN, schema, or SQL executor, you give it narrow tools such as:

- `get_customer`
- `search_orders`
- `check_inventory`
- `get_invoice_status`
- `list_recent_shipments`

Each tool is backed by a reviewed SQL statement, validated parameters, and execution limits. SELECT results use a field allow-list; mutations return affected count only.

This makes MCP useful without turning the legacy database into an unrestricted AI-accessible surface.

## Capability Boundary

`capability.yaml` is the agent-side security boundary and the agent's single source of executable operations.

It defines:

- service metadata
- agent runtime limits, including concurrent request execution
- gateway connection settings
- database connection fields
- agent private key
- default execution policies
- capability definitions
- parameter contracts
- SELECT result output contracts and the DML count-only contract
- local agent detail logging

A capability is a named business operation:

```yaml
capabilities:
  get_customer:
    description: Fetch one customer by id.
    sql: select id, name, email from customers where id = :customer_id
    params:
      customer_id:
        type: integer
        required: true
        minimum: 1
    policy:
      readonly: true
      timeout: 3s
      max_rows: 1
      max_bytes: 256KB
      expose_in_openapi: true
    result:
      id:
        type: integer
      name:
        type: string
      email:
        type: string
```

The agent loads and validates this file at startup, including checks for each SQL statement, before connecting to the gateway. Changes require an agent restart.

For the full schema, policy options, logging settings, and examples, see [Capability YAML](https://docs.onprest.viewlegacy.com/agent/capability-yaml).

## Security Model at a Glance

Onprest is designed as if components may be compromised.

- No SQL, DB credentials, DSNs, raw schema knowledge, capability execution rules, or agent private key are stored in the gateway.
- The gateway does not persist application data. It is configured with the agent public key and bcrypt-hashed API keys, and caches only agent-defined public capability metadata.
- Agent authentication uses Ed25519 signatures during the WebSocket handshake.
- The agent connects outbound to the gateway; no inbound firewall path into the customer network is required.
- API keys are capability-scoped.
- Unknown capability names are rejected by the agent.
- Parameters are validated before SQL runs.
- SQL parameters are bound through `database/sql`.
- Agent-owned policies restrict allowed operations, bound execution time, response size, and row counts, and control OpenAPI/MCP exposure.
- SELECT output fields are constrained by the `result` allow-list. DML cannot define `result` and returns only `{"count": n}`.
- Only one agent connection is accepted at a time.
- Gateway stdout logs do not include request params or agent error details.
- Detailed runtime error information is kept in the agent-local log.

For production deployments, use a read-only database user whenever the intended capabilities are read-only. Onprest's policy and validation layer is not a substitute for database-level least privilege; it is an additional control.

## Quick Start

Download the binary for your computer and the Quick Start files from the same v1.2.12 release (links become available when that release is published):

| Host | Download |
|---|---|
| Linux x64 | [⬇ Binary archive](https://github.com/viewlegacy/onprest/releases/download/v1.2.12/onprest-1.2.12-linux-amd64.tar.gz) |
| Linux ARM64 | [⬇ Binary archive](https://github.com/viewlegacy/onprest/releases/download/v1.2.12/onprest-1.2.12-linux-arm64.tar.gz) |
| macOS Intel | [⬇ Binary archive](https://github.com/viewlegacy/onprest/releases/download/v1.2.12/onprest-1.2.12-darwin-amd64.tar.gz) |
| macOS Apple silicon | [⬇ Binary archive](https://github.com/viewlegacy/onprest/releases/download/v1.2.12/onprest-1.2.12-darwin-arm64.tar.gz) |
| Windows x64 | [⬇ Binary archive](https://github.com/viewlegacy/onprest/releases/download/v1.2.12/onprest-1.2.12-windows-amd64.zip) |
| All hosts | [⬇ Quick Start files](https://github.com/viewlegacy/onprest/releases/download/v1.2.12/onprest-1.2.12-quickstart.tar.gz) |

On Linux x64, put both downloaded archives in one directory and run:

```sh
tar -xzf onprest-1.2.12-linux-amd64.tar.gz
tar -xzf onprest-1.2.12-quickstart.tar.gz
cd onprest-1.2.12-linux-amd64
QUICKSTART=../onprest-1.2.12-quickstart
docker compose -f "$QUICKSTART/postgres.compose.yml" up -d --wait
./onprest-agent validate --config "$QUICKSTART/capability.postgres.yaml"
set -a
. "$QUICKSTART/gateway.env"
set +a
./onprest-gateway
```

In a second terminal, change to the same binary directory and run `./onprest-agent --config ../onprest-1.2.12-quickstart/capability.postgres.yaml`. Once it connects, try:

```sh
curl -sS http://localhost:8080/healthz
curl -sS -H 'Authorization: Bearer orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM' \
  -H 'Content-Type: application/json' -d '{"customer_id":1}' \
  http://localhost:8080/api/v1/capabilities/get_customer
```

You should see `"agent_connected":true` and a row for Ada Lovelace. No source checkout, Go, or `make` is needed. Docker is only for the disposable example database; the [full Quick Start](https://docs.onprest.viewlegacy.com/quick-start) covers other hosts, MCP, and an existing evaluation PostgreSQL database. The included credentials are public examples—use fresh keys and the separate production templates for a real deployment.

## Build from Source

Building from source is optional. Use a Go toolchain compatible with the version declared in `go.mod`. CI reads `go.mod` as the Go version source of truth.

Build the two binaries:

```sh
make build
```

Cross-build gateway and agent binaries for common OS/CPU targets:

```sh
make build-cross
```

The binaries are built with `CGO_ENABLED=0` so they are suitable for copying to legacy environments without installing Docker or native database client libraries.

Docker is optional. The primary deployment unit is the binary.

## API Surface

Onprest exposes the same approved capabilities through REST and MCP.

- `POST /api/v1/capabilities/{name}` calls a capability with JSON params
- `POST /mcp` supports MCP `initialize`, `ping`, `tools/list`, and `tools/call`
- `GET /openapi.json` returns API-key-filtered OpenAPI
- `GET /healthz` returns gateway health and agent connection state

`/openapi.json` and MCP `tools/list` are generated from agent-owned capability metadata and filtered per API key.

## Supported Databases

Initial driver targets:

- PostgreSQL
- MySQL
- SQL Server
- Oracle

Oracle uses the pure-Go `go-ora` driver and does not require Oracle Instant Client.

## Documentation

Detailed docs are intentionally kept outside this README and published at
[docs.onprest.viewlegacy.com](https://docs.onprest.viewlegacy.com). The
source lives under [`docs/`](docs/).

Recommended next reads:

- [Architecture](https://docs.onprest.viewlegacy.com/architecture)
- [Capability YAML](https://docs.onprest.viewlegacy.com/agent/capability-yaml)
- [Gateway configuration](https://docs.onprest.viewlegacy.com/gateway/configuration)
- [Provisioning CLI](https://docs.onprest.viewlegacy.com/reference/cli)
- [REST API](https://docs.onprest.viewlegacy.com/api/rest)
- [MCP](https://docs.onprest.viewlegacy.com/api/mcp)
- [Security model](https://docs.onprest.viewlegacy.com/security)
- [Deployment](https://docs.onprest.viewlegacy.com/operations/deployment)
- [Operations](https://docs.onprest.viewlegacy.com/operations)

## Repository Boundary

This repository contains only the Apache-2.0 OSS core: `onprest-gateway` and `onprest-agent`. Managed products, dashboards, and their operating policies are outside this repository and are not OSS core dependencies.

## License

Apache-2.0. See [LICENSE](LICENSE).
