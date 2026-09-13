# Confirm an INSERT after losing its response

Define a read capability alongside a mutation so callers can check the database
when a response is lost. This example creates an order, deliberately discards a
successful INSERT response, and reads the order through a separate capability.
You can use PostgreSQL, MySQL, SQL Server, or Oracle. Additional steps let you
observe no row, changed contents, and an unavailable read.

The caller chooses a UNIQUE `external_request_id` before inserting and compares
all four fields when reading: request ID, product, quantity, and customer.
Finding the ID alone is insufficient. This example uses INSERT; see
[Mutation Reconciliation](https://docs.onprest.viewlegacy.com/operations/mutation-reconciliation)
for operational decisions and UPDATE/DELETE considerations.

## Before you start

Have Docker running, make, curl, a Unix shell, and the `onprest-gateway` and
`onprest-agent` binaries. You can use prebuilt binaries or run `make build`
from the repository root to build them in `dist/`. Go is needed only for
the response-loss exercise in step 3 (use the version in `go.mod`). SQL Server's
amd64 container requires amd64 emulation on an ARM Docker host.

Run the commands from the repository root, which contains `Makefile`.
Choose one database and the absolute path to your binaries:

```bash
export RECONCILIATION_DB=sqlserver
ONPREST_BIN_DIR=/absolute/path/to/onprest-binaries
```

`RECONCILIATION_DB` selects `postgres`, `mysql`, `sqlserver`, or `oracle`.
`ONPREST_BIN_DIR` is the directory containing the two executables.

## 1. Start the database and Onprest

```bash
make reconciliation-db
"$ONPREST_BIN_DIR/onprest-agent" validate \
  --config "examples/mutation-reconciliation/capability.$RECONCILIATION_DB.yaml"
```

The Makefile calls `db.sh` to start only the selected disposable container,
wait for it to be ready, apply `schema.<database>.sql`, and create
`capability_user` with INSERT and SELECT permissions (plus startup validation
permissions where needed). The table starts empty. Image download and database
startup can take several minutes. Without `RECONCILIATION_DB`, the Makefile
defaults to PostgreSQL.

Each `capability.<database>.yaml` is ready to use with the matching database.
All four use the same local Agent key pair and `gateway.env`; no configuration
generation or key copying is needed.

| Database | Container | Loopback port | Database/service |
|---|---|---|---|
| PostgreSQL | `onprest-recon-postgres` | `55433` | `reconcile` |
| MySQL | `onprest-recon-mysql` | `53306` | `reconcile` |
| SQL Server | `onprest-recon-sqlserver` | `51433` | `reconcile` |
| Oracle | `onprest-recon-oracle` | `51521` | `FREEPDB1` |

The YAML and env files contain fixed local example credentials. The database
helper initializes only its disposable containers. If a container name or port
is occupied, stop your previous example before starting another. For deployment,
use your own credentials and database settings and follow
[Deployment](https://docs.onprest.viewlegacy.com/operations/deployment).

Start the Gateway in another terminal, also from the repository root.
Set the same binary-directory path:

```bash
ONPREST_BIN_DIR=/absolute/path/to/onprest-binaries
set -a
. examples/mutation-reconciliation/gateway.env
set +a
"$ONPREST_BIN_DIR/onprest-gateway"
```

Start the Agent in a third terminal, from the repository root, using the
same binary directory and selected database:

```bash
ONPREST_BIN_DIR=/absolute/path/to/onprest-binaries
RECONCILIATION_DB=sqlserver
"$ONPREST_BIN_DIR/onprest-agent" \
  --config "examples/mutation-reconciliation/capability.$RECONCILIATION_DB.yaml"
```

In your original terminal, set the local example API key. It matches the hash
in `gateway.env`. Wait until health reports `agent_connected: true`:

```bash
GATEWAY_URL=http://127.0.0.1:58080
ONPREST_API_KEY='klJbEVYkjNUMXVN8PwysRdv4U3pDf0wtuS7_DMlGZdA'
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

In another terminal, run this from the repository root:

```bash
go run ./examples/mutation-reconciliation/response-loss.go
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

For an MCP response-loss exercise, restart `go run ./examples/mutation-reconciliation/response-loss.go`,
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

## 4. Try the other read results

These checks use REST with the same `reconcile_order` capability. MCP reads
return the same database state in `structuredContent`. Complete step 3 first;
the changed-contents check uses the `002` order from that response-loss exercise.

### No row

Read an unused request ID:

```bash
curl --fail-with-body -sS \
  -X POST "$GATEWAY_URL/api/v1/capabilities/reconcile_order" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"external_request_id":"req-20260906-004"}'
```

Expected result:

```json
{"rows":[],"count":0}
```

This shows how an empty read looks. No INSERT was sent for this ID, so this
step does **not** reproduce an unfinished INSERT or prove that an unknown
transaction rolled back. After a real lost response, a missing row can still
mean the earlier transaction is unfinished; resolve its state before deciding
whether to resend.

### Changed contents

Simulate another writer changing the `002` order after the INSERT:

```bash
make reconciliation-change-order
```

This uses the selected `RECONCILIATION_DB` container's administrative client to
set that order's quantity to `5`. The Agent user's permissions remain INSERT and
SELECT. Read through the Gateway again:

```bash
curl --fail-with-body -sS \
  -X POST "$GATEWAY_URL/api/v1/capabilities/reconcile_order" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"external_request_id":"req-20260906-002"}'
```

Expected result:

```json
{"rows":[{"external_request_id":"req-20260906-002","product_code":"SKU-001","quantity":5,"customer_code":"CUST-001"}],"count":1}
```

The ID exists, but the quantity differs from the original request's `2`. The
current order does not match the intended payload. Investigate the change
rather than resending the INSERT. This UPDATE only creates a competing-write
condition; it is not an UPDATE reconciliation example.

### Read unavailable

Stop the Agent with Ctrl-C in its terminal and leave the Gateway running.
Check health until it reports `agent_connected: false`:

```bash
curl --fail-with-body -sS "$GATEWAY_URL/healthz"
```

Then try the read:

```bash
curl --fail-with-body -sS \
  -X POST "$GATEWAY_URL/api/v1/capabilities/reconcile_order" \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"external_request_id":"req-20260906-002"}'
```

Expect HTTP 503 with `GATEWAY_AGENT_OFFLINE` (curl exit code 22). This is a
failed read, not an empty result; it tells you nothing about the order's presence
or contents. Restore read access before making an INSERT decision.

Restart the Agent in its terminal using the same binary directory and selected
database:

```bash
"$ONPREST_BIN_DIR/onprest-agent" \
  --config "examples/mutation-reconciliation/capability.$RECONCILIATION_DB.yaml"
```

Wait until health reports `agent_connected: true`, then repeat the SELECT above.
It should again return the `002` order with quantity `5`: read access is restored,
but the payload mismatch still needs investigation.

## 5. Clean up

Stop the Gateway and Agent with Ctrl-C (also stop the helper if it is still
waiting for a request). Then remove the selected example container and its
volumes, including the database:

```bash
make reconciliation-db-down
```

## Automated verification

`it/mutation_reconciliation_test.go` uses the same schemas and capability
YAML files, with REST/MCP requests defined in the test. It exercises response
cuts before and after commit, competing writes, UNIQUE conflicts, and read
unavailability on real databases. The local helper is only for the manual
post-completion exercise; it does not replace those tests.

To select just the SQL Server reconciliation scenarios, run from the repository root:

```bash
ONPREST_IT_REQUIRE_CONTAINERS=1 go test -tags=integration ./it \
  -run '^TestContainerDBDriverMutationReconciliation$' \
  -count=1 -timeout 30m -v -args -onprest-it-db=sqlserver
```

Change `sqlserver` to `postgres`, `mysql`, `oracle`, or `all`. The test starts and
cleans up its own containers independently of the manual example. See
[Test Commands](https://docs.onprest.viewlegacy.com/reference/test-commands)
for integration and release gates.
