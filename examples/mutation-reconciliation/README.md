# Check an order after a lost INSERT response

This example inserts an order and checks its contents through a separate read
capability. Use it for hands-on evaluation with PostgreSQL, MySQL, SQL Server,
or Oracle.

Onprest callers can execute only defined capabilities. Providing a
`reconcile_order` SELECT alongside `create_order` gives the caller a way to
check the order when the INSERT response is lost. Choose a UNIQUE
`external_request_id` before sending the INSERT, then compare all four fields:

| Field | Example value |
|---|---|
| `external_request_id` | `req-20260906-001` |
| `product_code` | `SKU-001` |
| `quantity` | `2` |
| `customer_code` | `CUST-001` |

This example covers INSERT reconciliation. UPDATE and DELETE need checks
appropriate to their business operation; this example does not supply them.

## Before you start

Have Docker running and the two Onprest binaries available. To build them from
the OSS repository, run `make build`; they are placed in `dist/`.
The commands below use a Unix shell and local Linux database containers. SQL
Server uses an amd64 image, so an ARM Docker host needs amd64 emulation enabled.

Run the commands from this directory. Choose `postgres`, `mysql`, `sqlserver`,
or `oracle`, and set the absolute path to your binaries:

```bash
RECONCILIATION_DB=sqlserver
ONPREST_BIN_DIR=/absolute/path/to/onprest-binaries
EXAMPLE_WORK_DIR=$(mktemp -d)
```

The local passwords below are example credentials. The containers are
disposable and expose ports only on loopback. Use a fresh example container;
if its name or port is occupied, choose another and update the commands and
configuration accordingly.

## 1. Prepare one database

Run only the subsection for your chosen database. Apply its schema with the
native client; Onprest does not apply it during startup. Each `seed.*.sql` file
is empty because the first INSERT creates the sample order.

### PostgreSQL

```bash
docker run -d --name onprest-recon-postgres \
  -p 127.0.0.1:55433:5432 \
  -e POSTGRES_DB=reconcile -e POSTGRES_USER=onprest_admin \
  -e POSTGRES_PASSWORD=Onprest-admin-1 postgres:16-alpine
```

Wait until this check succeeds before continuing:

```bash
docker exec -e PGPASSWORD=Onprest-admin-1 onprest-recon-postgres \
  psql -h 127.0.0.1 -U onprest_admin -d reconcile -Atc 'SELECT 1'
```

Apply the schema and create the Agent's user:

```bash
docker exec -i onprest-recon-postgres \
  psql -v ON_ERROR_STOP=1 -U onprest_admin -d reconcile < schema.postgres.sql

docker exec -i onprest-recon-postgres \
  psql -v ON_ERROR_STOP=1 -U onprest_admin -d reconcile <<'SQL'
CREATE ROLE capability_user LOGIN PASSWORD 'Onprest-example-1';
GRANT USAGE ON SCHEMA public TO capability_user;
GRANT SELECT, INSERT ON mutation_reconciliation_orders TO capability_user;
SQL
```

### MySQL

```bash
docker run -d --name onprest-recon-mysql \
  -p 127.0.0.1:53306:3306 \
  -e MYSQL_DATABASE=reconcile -e MYSQL_ROOT_PASSWORD=Onprest-admin-1 \
  mysql:8.0.36
```

Wait until this check succeeds:

```bash
docker exec -e MYSQL_PWD=Onprest-admin-1 onprest-recon-mysql \
  mysql -h 127.0.0.1 -u root reconcile -Nse 'SELECT 1'
```

Apply the schema and create the Agent's user:

```bash
docker exec -i -e MYSQL_PWD=Onprest-admin-1 onprest-recon-mysql \
  mysql -u root reconcile < schema.mysql.sql

docker exec -i -e MYSQL_PWD=Onprest-admin-1 onprest-recon-mysql \
  mysql -u root reconcile <<'SQL'
CREATE USER 'capability_user'@'%' IDENTIFIED BY 'Onprest-example-1';
GRANT SELECT, INSERT ON reconcile.mutation_reconciliation_orders TO 'capability_user'@'%';
SQL
```

### SQL Server

```bash
docker run -d --name onprest-recon-sqlserver --platform linux/amd64 \
  -p 127.0.0.1:51433:1433 \
  -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD=Onprest-admin-1 \
  mcr.microsoft.com/mssql/server:2022-CU14-ubuntu-22.04
```

Wait until this check succeeds. `-C` trusts this local container's certificate:

```bash
docker exec -e SQLCMDPASSWORD=Onprest-admin-1 onprest-recon-sqlserver \
  /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b -Q 'SELECT 1'
```

Create the database, apply the schema, and create the Agent's user. `SHOWPLAN`
is needed for startup validation, in addition to INSERT and SELECT permissions
for execution.

```bash
docker exec -e SQLCMDPASSWORD=Onprest-admin-1 onprest-recon-sqlserver \
  /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b \
  -Q 'CREATE DATABASE reconcile'

docker exec -i -e SQLCMDPASSWORD=Onprest-admin-1 onprest-recon-sqlserver \
  /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b -d reconcile \
  < schema.sqlserver.sql

docker exec -i -e SQLCMDPASSWORD=Onprest-admin-1 onprest-recon-sqlserver \
  /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b -d reconcile <<'SQL'
CREATE LOGIN capability_user WITH PASSWORD = 'Onprest-example-1';
CREATE USER capability_user FOR LOGIN capability_user;
GRANT SHOWPLAN TO capability_user;
GRANT SELECT, INSERT ON OBJECT::dbo.mutation_reconciliation_orders TO capability_user;
GO
SQL
```

### Oracle

```bash
docker run -d --name onprest-recon-oracle \
  -p 127.0.0.1:51521:1521 -e ORACLE_PASSWORD=Onprest-admin-1 \
  gvenzl/oracle-free:23-slim-faststart
```

Use `docker logs onprest-recon-oracle` to wait for the database-ready message.
Then apply the schema as SYSTEM and create a separate Agent user. Its private
synonym lets the template refer to the table without a schema qualifier.

```bash
docker cp schema.oracle.sql onprest-recon-oracle:/tmp/schema.oracle.sql

docker exec -i onprest-recon-oracle \
  sqlplus -s -L system/Onprest-admin-1@localhost:1521/FREEPDB1 <<'SQL'
WHENEVER SQLERROR EXIT FAILURE
@/tmp/schema.oracle.sql
CREATE USER capability_user IDENTIFIED BY "Onprest-example-1";
GRANT CREATE SESSION TO capability_user;
GRANT SELECT, INSERT ON system.mutation_reconciliation_orders TO capability_user;
CREATE SYNONYM capability_user.mutation_reconciliation_orders FOR system.mutation_reconciliation_orders;
EXIT
SQL
```

## 2. Configure and start Onprest

Copy the selected template into your temporary working directory:

```bash
cp "capability.$RECONCILIATION_DB.yaml.tmpl" "$EXAMPLE_WORK_DIR/capability.yaml"
```

Edit its `database` section using these values:

| Setting | PostgreSQL | MySQL | SQL Server | Oracle |
|---|---|---|---|---|
| `host` | `127.0.0.1` | `127.0.0.1` | `127.0.0.1` | `127.0.0.1` |
| `port` | `55433` | `53306` | `51433` | `51521` |
| `name` | `reconcile` | `reconcile` | `reconcile` | `FREEPDB1` |
| `user` | `capability_user` | `capability_user` | `capability_user` | `capability_user` |
| `password` | `Onprest-example-1` | `Onprest-example-1` | `Onprest-example-1` | `Onprest-example-1` |

Generate an Agent key pair and an API key scoped to both capabilities:

```bash
"$ONPREST_BIN_DIR/onprest-gateway" create-agent-secret
"$ONPREST_BIN_DIR/onprest-gateway" create-key \
  --name reconciliation --capabilities create_order,reconcile_order
```

Set `gateway.agent_private_key` in `capability.yaml` to the generated private
key. For this local example, set `gateway.url` to
`ws://127.0.0.1:58080/ws/agent`. Use `wss://` for non-loopback deployments as
described in [Deployment](https://docs.onprest.viewlegacy.com/operations/deployment).

Create `$EXAMPLE_WORK_DIR/gateway.env` with the generated public key and API key
hash substituted below. Keep the single quotes around the JSON to preserve
the `$` characters in the bcrypt hash when loading it through the shell.

```env
GATEWAY_ADDR=127.0.0.1:58080
GATEWAY_AGENT_PUBLIC_KEY=replace-with-generated-agent-public-key
GATEWAY_API_KEYS_JSON='[{"name":"reconciliation","key_hash":"replace-with-generated-key-hash","capabilities":["create_order","reconcile_order"]}]'
```

Validate the completed configuration:

```bash
"$ONPREST_BIN_DIR/onprest-agent" validate --config "$EXAMPLE_WORK_DIR/capability.yaml"
```

Start the Gateway in one terminal and the Agent in another. Set the same
`ONPREST_BIN_DIR` and `EXAMPLE_WORK_DIR` paths in both terminals:

```bash
set -a
. "$EXAMPLE_WORK_DIR/gateway.env"
set +a
"$ONPREST_BIN_DIR/onprest-gateway"
```

```bash
"$ONPREST_BIN_DIR/onprest-agent" --config "$EXAMPLE_WORK_DIR/capability.yaml"
```

In the request terminal, set the generated plaintext API key and check that
`/healthz` reports `agent_connected: true`:

```bash
GATEWAY_URL=http://127.0.0.1:58080
ONPREST_API_KEY=replace-with-generated-api-key
curl --fail-with-body -sS "$GATEWAY_URL/healthz"
```

## 3. Insert and check the order

Choose REST or MCP for the initial INSERT. Both files use the same request ID,
so use the other protocol only for reading if you already inserted it. For
another business operation, choose a new ID in the form `req-YYYYMMDD-NNN` and
update the create and reconcile JSON files together.

### REST

Insert once:

```bash
curl --fail-with-body -sS \
  -X POST "$GATEWAY_URL/api/v1/capabilities/create_order" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d @rest-create.json
```

Success returns `{"count":1}`. Check the stored order with the read capability;
this is also the request to use if the INSERT response is lost:

```bash
curl --fail-with-body -sS \
  -X POST "$GATEWAY_URL/api/v1/capabilities/reconcile_order" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d @rest-reconcile.json
```

A matching result is:

```json
{"rows":[{"external_request_id":"req-20260906-001","product_code":"SKU-001","quantity":2,"customer_code":"CUST-001"}],"count":1}
```

Compare all four fields with the request. Finding the ID alone does not confirm
that the order has the intended contents.

### MCP

If you have not sent the REST INSERT, insert once with MCP:

```bash
curl --fail-with-body -sS \
  -X POST "$GATEWAY_URL/mcp" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "MCP-Protocol-Version: 2025-11-25" \
  -d @mcp-create.json
```

Read the order, regardless of which protocol performed the INSERT:

```bash
curl --fail-with-body -sS \
  -X POST "$GATEWAY_URL/mcp" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "MCP-Protocol-Version: 2025-11-25" \
  -d @mcp-reconcile.json
```

The INSERT returns a count-only tool result. The read returns the same order
data shown above in `structuredContent` and the text content. Check the body:
a tool execution error has `isError: true` even with HTTP 200.

If a response is lost, keep the original request ID and read first rather than
automatically sending the INSERT again. Use the
[Mutation Reconciliation decision table](https://docs.onprest.viewlegacy.com/operations/mutation-reconciliation#order-insert-example)
to distinguish a full match, no row, differing contents, and an unavailable
read. A UNIQUE constraint error confirms rollback of that attempt; read the
existing order to determine whether its contents match your request.

## 4. Clean up

Stop the Gateway and Agent with Ctrl-C. Remove only the container created for
this example; removing it also discards the example database:

```bash
docker rm -f "onprest-recon-$RECONCILIATION_DB"
```

Remove the temporary `capability.yaml` and `gateway.env` when finished. Keep
them out of source control. If using an existing sample database instead,
drop `mutation_reconciliation_orders` manually after checking the target schema
(`dbo.mutation_reconciliation_orders` on SQL Server, and
`system.mutation_reconciliation_orders` for the Oracle setup above).

## Automated verification

The integration suite reads these same schema, template, and JSON files and
uses temporary databases to exercise response cuts, competing writes, and read
unavailability. The manual calls above do not inject a connection failure.

To run only the SQL Server reconciliation scenarios, run this from the OSS
repository root:

```bash
ONPREST_IT_REQUIRE_CONTAINERS=1 go test -tags=integration ./it \
  -run '^TestContainerDBDriverMutationReconciliation$' \
  -count=1 -timeout 30m -v -args -onprest-it-db=sqlserver
```

Change `sqlserver` to another supported database or `all`. This command starts
and cleans up its own containers; it does not use the manually started database.
See [Test Commands](https://docs.onprest.viewlegacy.com/reference/test-commands)
for the wider integration and release gates.
