# Onprest

Onprest is an **agent-defined capability tunnel** for legacy databases.

It exposes selected on-prem-defined business capabilities through REST and MCP, without exposing SQL, database credentials, raw schema access, cloud data replicas, or inbound firewall ports.

> Keep the legacy system. Modernize the access layer.  
> Expose capabilities, not your database.

Most AI/database integrations start from the wrong primitive: database access.

Onprest starts from a smaller primitive: a named business capability. AI agents, SaaS products, internal tools, and partner systems call explicit operations such as `get_customer`, `search_orders`, or `check_inventory`. They never receive a DSN, raw SQL access, or a schema-wide CRUD surface.

The public gateway handles routing, identity, rate limits, and observability. The on-prem agent owns SQL, credentials, validation, execution policy, output filtering, and business meaning. `capability.yaml` defines the only operations that can exist.

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

The agent knows what each capability means, how parameters are validated, which prepared SQL is executed, and which result fields are allowed to leave the customer environment.

TLS termination is environment-owned. Use any reverse proxy, platform proxy, or load balancer that supports HTTPS and WebSocket forwarding.

## Responsibility Split

The gateway can be useful without being trusted with meaning.

| Concern | Gateway | On-prem Agent |
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
| Output allow-list | No | Yes |
| Detailed DB errors | No | Yes, local only |
| Agent private key | No | Yes |
| Agent public key | Yes | No |

This split is the core of Onprest. The gateway is public and operationally useful, but intentionally does not hold the most sensitive parts of the integration.

## Trust Boundary

Onprest assumes the public gateway may be observed or compromised.

That is why the gateway never stores SQL, DSNs, database credentials, agent private keys, raw schema knowledge, or business meaning. It can authenticate callers, apply rate limits, check whether an API key may call a capability name, and forward the request to the connected agent.

The on-prem agent is the trust boundary. It validates inputs, applies execution policy, executes prepared SQL, filters outputs, and keeps detailed DB errors local.

In short:

- compromise the gateway: you do not get SQL or database credentials
- compromise an API key: you only get the capabilities assigned to that key
- ask for an unknown operation: the agent rejects it
- return extra columns from SQL: `result` filters them before they leave the agent
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

Each tool is backed by a reviewed SQL statement, validated parameters, row and byte limits, timeout policy, and result-field allow-list.

This makes MCP useful without turning the legacy database into an unrestricted AI-accessible surface.

## Capability Boundary

`capability.yaml` is the agent-side security boundary and the agent's single source of executable operations.

It defines:

- service metadata
- gateway connection settings
- database connection fields
- agent private key
- default execution policies
- capability definitions
- parameter contracts
- result output contracts
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

The agent loads this file at startup, validates it, runs SQL checks, and then connects to the gateway. Changes require an agent restart.

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
- Policies can constrain readonly mode, timeout, max rows, max bytes, and OpenAPI/MCP exposure.
- Output fields are constrained by the `result` allow-list.
- Only one agent connection is accepted at a time.
- Gateway stdout logs do not include request params or agent error details.
- Detailed runtime error information is kept in the agent-local log.

For production deployments, use a read-only database user whenever the intended capabilities are read-only. Onprest's policy and validation layer is not a substitute for database-level least privilege; it is an additional control.

## Quick Start

Quick Start assumes you are running from the repository root.

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
`legacy` database, creates the `readonly_user` account used by
`examples/capability.postgres.yaml`, and seeds the `customers` table. If
`127.0.0.1:5432` is already in use, stop that PostgreSQL instance or edit
both `examples/postgres.compose.yml` and `examples/capability.postgres.yaml`
to use the same alternate port.

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

Set the example API key in the shell that will call the gateway:

```sh
export API_KEY='orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM'
```

Call a capability:

```sh
curl -sS \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"customer_id":1}' \
  http://localhost:8080/api/v1/capabilities/get_customer
```

The example database is initialized from [`examples/postgres-init.sql`](examples/postgres-init.sql).

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

Oracle requires the runtime prerequisites for `godror`.

## Documentation

Detailed docs are intentionally kept outside this README.

Recommended next reads:

- [Architecture](docs/app/architecture/page.mdx)
- [Capability YAML](docs/app/agent/capability-yaml/page.mdx)
- [Gateway configuration](docs/app/gateway/configuration/page.mdx)
- [Provisioning CLI](docs/app/reference/cli/page.mdx)
- [REST API](docs/app/api/rest/page.mdx)
- [MCP](docs/app/api/mcp/page.mdx)
- [Security model](docs/app/security/page.mdx)
- [Deployment](docs/app/operations/deployment/page.mdx)
- [Operations](docs/app/operations/page.mdx)

## OSS and Managed

Onprest is dual-distributed:

- **OSS (this repository, Apache-2.0)**: self-host the gateway and agent on your own infrastructure.
- **Managed**: we operate the customer-dedicated gateway, monitor agent connectivity, handle patching, retain operational logs, and take operational responsibility around the connection layer.

Both share the same OSS core. You can move from managed to self-hosted at any time using the same binaries and `capability.yaml`.

Managed Onprest is for teams that want the architecture without owning the operations. Your database, SQL, credentials, and capability definitions stay under your control.

## License

Apache-2.0. See [LICENSE](LICENSE).
