# Onprest

Onprest lets you expose selected business operations from a legacy database
through REST and MCP—without exposing the database itself.

> Keep the legacy system. Modernize the access layer.  
> Expose capabilities, not your database.

Instead of handing an AI agent or another application a database connection,
you define named operations such as `get_customer` or `search_orders` beside
the database. Callers can use only the capabilities you choose to publish.

## How It Works

```text
API user / MCP client
        |
        | REST / MCP
        v
onprest-gateway
        ^
        | outbound connection from on-prem
        |
onprest-agent + capability.yaml
        |
        v
legacy database
```

The Gateway handles API keys, routing, and public endpoints. The Agent owns
the database connection, SQL, input validation, and the results allowed to
leave the private network. No inbound firewall path to the database network
is needed. Onprest is not a SQL proxy or a schema-wide CRUD generator.

Both components are independent binaries. Supported database drivers are
PostgreSQL, MySQL, SQL Server, and Oracle. Capabilities are available through
REST and as named MCP tools, so AI clients never need a DSN or raw SQL access.

## Download

Choose the archive for your OS/CPU from a [GitHub Release](https://github.com/viewlegacy/onprest/releases).
Each archive contains both the Gateway and Agent. The buttons below open the
latest release page; select the matching asset name there. Prebuilt archives
start with v1.2.12; earlier releases do not have these files.

| Host | Download | Asset name |
|---|---|---|
| Linux x64 | [⬇ Linux x64](https://github.com/viewlegacy/onprest/releases/latest) | `onprest-X.Y.Z-linux-amd64.tar.gz` |
| Linux ARM64 | [⬇ Linux ARM64](https://github.com/viewlegacy/onprest/releases/latest) | `onprest-X.Y.Z-linux-arm64.tar.gz` |
| macOS Apple silicon | [⬇ macOS Apple silicon](https://github.com/viewlegacy/onprest/releases/latest) | `onprest-X.Y.Z-darwin-arm64.tar.gz` |
| macOS Intel | [⬇ macOS Intel](https://github.com/viewlegacy/onprest/releases/latest) | `onprest-X.Y.Z-darwin-amd64.tar.gz` |
| Windows x64 | [⬇ Windows x64](https://github.com/viewlegacy/onprest/releases/latest) | `onprest-X.Y.Z-windows-amd64.zip` |
| Local trial files | [⬇ Quick Start](https://github.com/viewlegacy/onprest/releases/latest) | `onprest-X.Y.Z-quickstart.tar.gz` |

Use the binary archive and Quick Start asset from the **same release**. The
Quick Start asset contains a disposable PostgreSQL setup and public example
credentials. It is for local evaluation only, not production configuration.

## Quick Start

This Linux/macOS walkthrough uses prebuilt binaries—no source checkout, Go,
or `make`. Docker Compose supplies the disposable example database; if you
already have an empty evaluation PostgreSQL database, the
[full Quick Start](https://docs.onprest.viewlegacy.com/quick-start) explains
how to use it without Docker. Windows users can follow the PowerShell section
there after downloading the two Windows-compatible assets above.

Download the two archives from one release, replace `X.Y.Z` and
`<os>-<arch>` below with their filenames, and extract them side by side:

```bash
tar -xzf "onprest-X.Y.Z-<os>-<arch>.tar.gz"
tar -xzf "onprest-X.Y.Z-quickstart.tar.gz"
cd "onprest-X.Y.Z-<os>-<arch>"
```

Start the example database and check the Agent configuration:

```bash
docker compose -f ../onprest-X.Y.Z-quickstart/postgres.compose.yml up -d --wait
./onprest-agent validate --config ../onprest-X.Y.Z-quickstart/capability.postgres.yaml
```

Start the Gateway in this terminal:

```bash
set -a
. ../onprest-X.Y.Z-quickstart/gateway.env
set +a
./onprest-gateway
```

Open another terminal in the same extracted binary directory and start the
Agent:

```bash
./onprest-agent --config ../onprest-X.Y.Z-quickstart/capability.postgres.yaml
```

Then call the example `get_customer` capability:

```bash
curl -sS http://127.0.0.1:8080/healthz
curl -sS -H 'Authorization: Bearer orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM' \
  -H 'Content-Type: application/json' -d '{"customer_id":1}' \
  http://127.0.0.1:8080/api/v1/capabilities/get_customer
```

The health response should show `"agent_connected": true`, and the REST
response should contain Ada Lovelace. The same capabilities are available
through MCP; the [full Quick Start](https://docs.onprest.viewlegacy.com/quick-start)
includes that call, Windows commands, troubleshooting, and cleanup. Stop the
two binaries with Ctrl+C. To remove the example database and its volume:

```bash
docker compose -f ../onprest-X.Y.Z-quickstart/postgres.compose.yml down -v
```

Never reuse the Quick Start keys or database password in a real deployment.
For production, use the separate templates inside the OS archive and follow
the [deployment guide](https://docs.onprest.viewlegacy.com/operations/deployment).

## Define Your Own Capabilities

`capability.yaml` is where you decide which operations exist. A capability
has a name, validated parameters, SQL, execution policy, and—for reads—the
fields allowed in the response. Mutations return an affected count, not rows.
See [Capability YAML](https://docs.onprest.viewlegacy.com/agent/capability-yaml)
for the full format and database-specific examples.

The Agent validates its configuration and database access before connecting
to the Gateway. Use a database account with only the permissions those
capabilities need.

## Learn More

- [Architecture](https://docs.onprest.viewlegacy.com/architecture) and [security](https://docs.onprest.viewlegacy.com/security)
- [Gateway configuration](https://docs.onprest.viewlegacy.com/gateway/configuration) and [Agent capability YAML](https://docs.onprest.viewlegacy.com/agent/capability-yaml)
- [REST API](https://docs.onprest.viewlegacy.com/api/rest) and [MCP](https://docs.onprest.viewlegacy.com/api/mcp)
- [Deployment](https://docs.onprest.viewlegacy.com/operations/deployment) and [operations](https://docs.onprest.viewlegacy.com/operations)

Developers can build from source with `make build`; see the
[CLI reference](https://docs.onprest.viewlegacy.com/reference/cli#build-from-source).
The OSS core is Apache-2.0 licensed. See [LICENSE](LICENSE).
