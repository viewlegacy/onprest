# Confirm an INSERT after losing its response

Define a read capability alongside a mutation so callers can check the database
when a response is lost. This example creates an order, deliberately discards a
successful INSERT response, and reads the order through a separate capability.
You can use PostgreSQL, MySQL, SQL Server, or Oracle.

The caller chooses a UNIQUE `external_request_id` before inserting and compares
all four fields when reading: request ID, product, quantity, and customer.
Finding the ID alone is insufficient. This example uses INSERT; see
[Mutation Reconciliation](https://docs.onprest.viewlegacy.com/operations/mutation-reconciliation)
for operational decisions and UPDATE/DELETE considerations.

## Before you start

Have Docker running, Go (the version in the repository's `go.mod`), curl, a
Unix shell, and the `onprest-gateway` and `onprest-agent` binaries. You can use
prebuilt binaries or run `make build` from the OSS repository root to build them
in `dist/`. Go runs the local example helper; it is not required to deploy the
Gateway or Agent. SQL Server's amd64 container requires amd64 emulation on an
ARM Docker host.

Run these commands from `examples/mutation-reconciliation`. Choose one database
and the absolute path to your binaries:

```bash
RECONCILIATION_DB=sqlserver
ONPREST_BIN_DIR=/absolute/path/to/onprest-binaries
EXAMPLE_WORK_DIR=$(mktemp -d)
```

`RECONCILIATION_DB` selects `postgres`, `mysql`, `sqlserver`, or `oracle`.
`ONPREST_BIN_DIR` is the directory containing the two executables.
`EXAMPLE_WORK_DIR` holds generated configuration and credentials for this run.
Keep it out of source control.

## 1. Start the database and prepare Onprest

```bash
./db.sh up "$RECONCILIATION_DB"
go run ./local.go configure "$RECONCILIATION_DB" "$EXAMPLE_WORK_DIR" "$ONPREST_BIN_DIR"
```

The first command starts only the selected disposable container, waits for it
to be ready, applies `schema.<database>.sql`, and creates `capability_user` with
INSERT and SELECT permissions (plus startup validation permissions where
needed). The table starts empty. Image download and database startup can take
several minutes.

The second command fills the selected capability template, generates a fresh
Agent key pair and an API key scoped to `create_order` and `reconcile_order`,
and validates the configuration against the database. No keys need copying.
It writes these private files to the working directory:

- `capability.yaml`: the Agent configuration with the two capabilities.
- `gateway.env`: Gateway settings and the binary/working-directory paths.
- `request.env`: Gateway URL and the plaintext API key used by curl.

The fixed local connections are:

| Database | Container | Loopback port | Database/service |
|---|---|---|---|
| PostgreSQL | `onprest-recon-postgres` | `55433` | `reconcile` |
| MySQL | `onprest-recon-mysql` | `53306` | `reconcile` |
| SQL Server | `onprest-recon-sqlserver` | `51433` | `reconcile` |
| Oracle | `onprest-recon-oracle` | `51521` | `FREEPDB1` |

The templates already contain these connections and the local sample user and
password. The helper targets these containers; it does not initialize an
existing database. If a container name or port is occupied, stop your previous
example before starting another. The local credentials and loopback-only
`ws://` connection are for this disposable example. For deployment, use your
own database settings and follow [Deployment](https://docs.onprest.viewlegacy.com/operations/deployment).

Start the Gateway in another terminal. Substitute the actual working-directory
path printed by `configure`:

```bash
EXAMPLE_WORK_DIR=/actual/path/printed/by/configure
set -a
. "$EXAMPLE_WORK_DIR/gateway.env"
set +a
"$ONPREST_BIN_DIR/onprest-gateway"
```

Start the Agent in a third terminal with the same working-directory path:

```bash
EXAMPLE_WORK_DIR=/actual/path/printed/by/configure
. "$EXAMPLE_WORK_DIR/gateway.env"
"$ONPREST_BIN_DIR/onprest-agent" --config "$EXAMPLE_WORK_DIR/capability.yaml"
```

In your original terminal, load the request settings and wait until health
reports `agent_connected: true`:

```bash
. "$EXAMPLE_WORK_DIR/request.env"
curl --fail-with-body -sS "$GATEWAY_URL/healthz"
```

## 2. Insert and read normally

Insert once using REST:

```bash
curl --fail-with-body -sS \
  -X POST "$GATEWAY_URL/api/v1/capabilities/create_order" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"external_request_id":"req-20260906-001","product_code":"SKU-001","quantity":2,"customer_code":"CUST-001"}'
```

Success returns `{"count":1}`. Read through the separate capability:

```bash
curl --fail-with-body -sS \
  -X POST "$GATEWAY_URL/api/v1/capabilities/reconcile_order" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"external_request_id":"req-20260906-001"}'
```

Expected result:

```json
{"rows":[{"external_request_id":"req-20260906-001","product_code":"SKU-001","quantity":2,"customer_code":"CUST-001"}],"count":1}
```

## 3. Lose a response, then confirm the order

In another terminal, run this from the example directory:

```bash
go run ./local.go lose-response
```

Wait for `Ready on http://127.0.0.1:58081`. This local helper forwards one INSERT
to the Gateway on port `58080`. After receiving a successful one-row INSERT
response, it closes the caller's connection without delivering that response
and exits. It does not retry. This reproduces response loss **after completion**;
Agent disconnection and unfinished transactions are covered by the integration
suite, not this manual step.

In the original terminal, insert a different order through the helper:

```bash
curl --fail-with-body -sS \
  -X POST "http://127.0.0.1:58081/api/v1/capabilities/create_order" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"external_request_id":"req-20260906-002","product_code":"SKU-001","quantity":2,"customer_code":"CUST-001"}'
```

curl reports an empty response (normally exit code 52). **Do not repeat this
INSERT.** Keep its request ID and read directly through the Gateway:

```bash
curl --fail-with-body -sS \
  -X POST "$GATEWAY_URL/api/v1/capabilities/reconcile_order" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"external_request_id":"req-20260906-002"}'
```

The result should contain the `002` order with all four fields matching your
request. You have confirmed that the intended order is present despite losing
the INSERT response. For no row, differing contents, or a failed read, use the
[operational decision table](https://docs.onprest.viewlegacy.com/operations/mutation-reconciliation#order-insert-example).
Do not interpret every missing response as a successful commit.

### Use MCP instead

For an MCP response-loss exercise, restart `go run ./local.go lose-response`,
wait for its ready message, and send a new request ID:

```bash
curl --fail-with-body -sS \
  -X POST "http://127.0.0.1:58081/mcp" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "MCP-Protocol-Version: 2025-11-25" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_order","arguments":{"external_request_id":"req-20260906-003","product_code":"SKU-001","quantity":2,"customer_code":"CUST-001"}}}'
```

Read directly through the Gateway:

```bash
curl --fail-with-body -sS \
  -X POST "$GATEWAY_URL/mcp" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "MCP-Protocol-Version: 2025-11-25" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"reconcile_order","arguments":{"external_request_id":"req-20260906-003"}}}'
```

The read returns the `003` order in `structuredContent` and text content.
MCP tool errors can use HTTP 200 with `isError: true`; inspect the body. The
helper discards only a successful one-row INSERT response, so an upstream
execution error is returned instead of being presented as simulated success.
Use a fresh request ID or reset the example database to repeat an exercise.

## 4. Clean up

Stop the Gateway and Agent with Ctrl-C (also stop the helper if it is still
waiting for a request). Then remove the selected example container and its
volumes, including the database:

```bash
./db.sh down "$RECONCILIATION_DB"
```

Delete the three generated files in `EXAMPLE_WORK_DIR` when finished. A fresh
working directory is required for the next `configure` run.

## Automated verification

`it/mutation_reconciliation_test.go` uses the same schemas and capability
templates, with REST/MCP requests defined in the test. It exercises response
cuts before and after commit, competing writes, UNIQUE conflicts, and read
unavailability on real databases. The local helper is only for the manual
post-completion exercise; it does not replace those tests.

To select just the SQL Server reconciliation scenarios, run from the OSS root:

```bash
ONPREST_IT_REQUIRE_CONTAINERS=1 go test -tags=integration ./it \
  -run '^TestContainerDBDriverMutationReconciliation$' \
  -count=1 -timeout 30m -v -args -onprest-it-db=sqlserver
```

Change `sqlserver` to `postgres`, `mysql`, `oracle`, or `all`. The test starts and
cleans up its own containers independently of the manual example. See
[Test Commands](https://docs.onprest.viewlegacy.com/reference/test-commands)
for integration and release gates.
