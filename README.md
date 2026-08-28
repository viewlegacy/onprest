# Onprest

Onprest is an **agent-defined capability tunnel** for legacy databases.

It exposes selected on-prem-defined business capabilities through REST and MCP, without exposing SQL, database credentials, raw schema access, cloud data replicas, or inbound firewall ports.

> Keep the legacy system. Modernize the access layer.  
> Expose capabilities, not your database.

Most AI/database integrations start from the wrong primitive: database access.

Onprest starts from a smaller primitive: a named business capability. AI agents, SaaS products, internal tools, and partner systems call explicit operations such as `get_customer`, `search_orders`, or `check_inventory`. They never receive a DSN, raw SQL access, or a schema-wide CRUD surface.

The public gateway handles routing, identity, rate limits, and observability. The on-prem agent owns SQL, credentials, validation, execution policy, SELECT output filtering, the DML count-only contract, and business meaning. `capability.yaml` defines the only operations that can exist.

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
        | HTTPS
        v
reverse proxy / load balancer
        |
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

The gateway knows routing, identity, rate limits, and which API keys may call which capability names.

The agent knows what each capability means, how parameters are validated, which prepared SQL is executed, and—for SELECT—which result fields are allowed to leave the customer environment. Mutations return affected count only.

TLS termination is environment-owned. Use any reverse proxy, platform proxy, or load balancer that supports HTTPS and WebSocket forwarding.

## Responsibility Split

The gateway can be useful without being trusted with meaning.

| Concern | Gateway | Agent |
|---|---:|---:|
| Public REST/MCP endpoint | Yes | No |
| WebSocket edge | Yes | Outbound client |
| API key authentication | Yes | No |
| Capability authorization | Yes | Yes |
| Rate limiting | Yes | No |
| Request observability | Yes | No |
| OpenAPI/MCP filtering by API key | Yes | No |
| SQL text | No | Yes |
| Database credentials | No | Yes |
| Database schema knowledge | No | Yes |
| Business meaning | No | Yes |
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

That is why the gateway never stores SQL, DSNs, database credentials, agent private keys, raw schema knowledge, or business meaning. It can authenticate callers, apply rate limits, check whether an API key may call a capability name, and forward the request to the connected agent.

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

Each tool is backed by a reviewed SQL statement, validated parameters, timeout and byte limits, plus SELECT-only row limits and result-field allow-list. DML ignores `max_rows`, forbids `result`, and returns only native affected count.

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

The agent loads this file at startup, validates it, runs SQL checks, and then connects to the gateway. `onprest-agent validate --config PATH` runs the same startup preflight without connecting to the gateway. Changes require an agent restart.

For the full schema, policy options, logging settings, and examples, see the documentation.

## Security Model at a Glance

Onprest is designed as if components may be compromised.

- No SQL, DB credentials, DSNs, raw schema knowledge, business meaning, or agent private key are stored in the gateway.
- The gateway stores only the agent public key and bcrypt-hashed API keys.
- Agent authentication uses Ed25519 signatures during the WebSocket handshake.
- The agent connects outbound to the gateway; no inbound firewall path into the customer network is required.
- API keys are capability-scoped.
- Unknown capability names are rejected by the agent.
- Parameters are validated before SQL runs.
- SQL parameters are bound through `database/sql`.
- Policies can constrain readonly mode, timeout, max bytes, and OpenAPI/MCP exposure; `max_rows` applies only to SELECT.
- SELECT output fields are constrained by the `result` allow-list. DML cannot define `result` and returns only `{"count": n}`.
- Only one agent connection is accepted at a time.
- Gateway stdout logs do not include request params or agent error details.
- Detailed runtime error information is kept in the agent-local log.

For production deployments, use a read-only database user whenever the intended capabilities are read-only. Onprest's policy and validation layer is not a substitute for database-level least privilege; it is an additional control.

## Quick Start

### Prerequisites

- A Go toolchain compatible with the version declared in [`go.mod`](go.mod), to build the binaries.
- Docker with Compose, used only to start the disposable example PostgreSQL database in this guide. Docker is not required to run Onprest itself.
- A Linux or macOS host. `make build` builds natively for the host you run it on, so on Linux it produces Linux binaries and on macOS it produces macOS binaries. Use `make build-cross` to produce binaries for other targets (additional Linux/macOS architectures and Windows).

Quick Start assumes you are running from the repository root, and uses the repository example files so you do not have to generate keys yet.

Build the two binaries:

```sh
make build
```

This creates:

```text
dist/onprest-gateway
dist/onprest-agent
```

Start the example PostgreSQL database with Docker Compose:

```sh
make quickstart-db
```

This starts a local PostgreSQL container on `127.0.0.1:5432`, creates the
`legacy` database, creates the least-privilege `capability_user` account used by
`examples/capability.postgres.yaml`, and seeds the `customers` table. If
`127.0.0.1:5432` is already in use, stop that PostgreSQL instance or edit
both `examples/postgres.compose.yml` and `examples/capability.postgres.yaml`
to use the same alternate port.

Validate the Agent configuration and database preflight before starting either
process:

```sh
./dist/onprest-agent validate --config examples/capability.postgres.yaml
```

Success confirms full YAML lint, database Ping, and every capability's
driver-specific EXPLAIN. It does not connect to the Gateway or execute a
business capability.

Start the gateway:

```sh
set -a
. examples/gateway.env
set +a
./dist/onprest-gateway
```

Start the agent in another shell:

```sh
./dist/onprest-agent --config examples/capability.postgres.yaml
```

Set the example API key in the shell that will call the gateway. This is the plaintext key that matches the bcrypt hash already present in `examples/gateway.env`. For your own deployment, generate keys with `onprest-gateway create-key` (see [Provisioning CLI](https://docs.onprest.viewlegacy.com/reference/cli)).

```sh
export ONPREST_API_KEY='orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM'
```

Confirm the gateway is up and the agent is connected before calling a capability:

```sh
curl -sS http://localhost:8080/healthz
```

```json
{ "ok": true, "agent_connected": true }
```

`/healthz` needs no API key, but it still uses the configured IP allow list and per-source rate limit. If `agent_connected` is `false`, wait for the agent to finish its startup checks and connect, then retry.

Call a capability:

```sh
curl -sS \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"customer_id":1}' \
  http://localhost:8080/api/v1/capabilities/get_customer
```

A successful call returns the rows the capability is allowed to expose:

```json
{
  "rows": [
    { "id": 1, "name": "Ada Lovelace", "email": "ada@example.com" }
  ],
  "count": 1
}
```

Only the fields listed in the capability `result` allow-list are returned. The example database is initialized from [`examples/postgres-init.sql`](examples/postgres-init.sql).

Run the shipped mutation over REST:

```sh
curl -sS \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"customer_id":1,"name":"Ada REST"}' \
  http://localhost:8080/api/v1/capabilities/update_customer
```

Then run the same capability through MCP:

```sh
curl -sS \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"update_customer","arguments":{"customer_id":1,"name":"Ada MCP"}}}' \
  http://localhost:8080/mcp
```

Both calls return the driver's native affected count. Verify the committed state through the read capability:

```sh
curl -sS \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"customer_id":1}' \
  http://localhost:8080/api/v1/capabilities/get_customer
```

The returned name is `Ada MCP`. The shipped API key allow-list contains only `get_customer` and `update_customer`; the DB role has only `SELECT` and column-level `UPDATE (name)` on `customers`.

For a direct check of the disposable database:

```sh
docker compose -f examples/postgres.compose.yml exec -T postgres \
  psql -U onprest_admin -d legacy -Atc "SELECT name FROM customers WHERE id = 1"
```

This also prints `Ada MCP`.

Stop and remove the example database when finished:

```sh
make quickstart-db-down
```

For real deployments, edit `capability.yaml` and place it beside `onprest-agent`.

## Build

Use a Go toolchain compatible with the version declared in `go.mod`. CI reads `go.mod` as the Go version source of truth.

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
- `GET /ws/agent` is reserved for the outbound agent WebSocket

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
