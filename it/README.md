# Integration Tests

Integration tests are build-tagged and do not run with the default unit test command.

DB-backed integration tests always start temporary DB containers with testcontainers, render the container connection details into the test capability YAML, and then run the agent against that YAML. Self-hosted DB overrides through `ONPREST_IT_POSTGRES_*`, `ONPREST_IT_MYSQL_*`, `ONPREST_IT_SQLSERVER_*`, `ONPREST_IT_ORACLE_*`, or `ONPREST_IT_POSTGRES_READONLY_*` are intentionally not supported.

The four-database matrix includes SELECT compatibility plus INSERT/UPDATE/DELETE native affected counts, zero-row success, MCP mutation, constraint normalization, startup EXPLAIN behavior, and persisted DB state. Container-required gates must use `ONPREST_IT_REQUIRE_CONTAINERS=1`; a skipped DB is not a release pass.

## Main Commands

Use these for normal development and release decisions.

```bash
make test-it
```

Development IT check. This runs the PostgreSQL integration path by default. It starts PostgreSQL with testcontainers and writes the discovered container connection details into the test capability YAML. It does not run MySQL, SQL Server, Oracle, Docker image, or Docker Compose operational cases.

```bash
make test-it-postgres-ci
```

PostgreSQL operation-readiness gate. This runs the PostgreSQL integration suite three times with `ONPREST_IT_REQUIRE_CONTAINERS=1`, so a PostgreSQL container startup failure is a test failure instead of a skip.

```bash
make test-it-release-gate
```

Final OSS core release gate. This runs unit tests, PostgreSQL CI, PostgreSQL DB interruption stability, all-DB smoke, the MySQL special-credential connection, SQL Server real TLS verification, Docker image operations, Docker Compose operations, test/subtest skip detection, and testcontainers leftover detection. Package-level `go test -json` skip records without a `Test` field are not test skips.

## Support Commands

Use these when touching a specific area.

```bash
make test-it-postgres-stability
```

Runs `TestPostgresDBUnreachableDuringQuery` five times. Use this when changing DB interruption, DB reachability, or error classification behavior. The test waits until PostgreSQL reports the long-running query as active before stopping the container.

```bash
make test-it-all-db
```

Runs every integration test whose name begins `TestContainerDBDriver` for PostgreSQL, MySQL, SQL Server, and Oracle with `ONPREST_IT_REQUIRE_CONTAINERS=1`, the exact Makefile filter `-run '^TestContainerDBDriver'`, and a 30-minute timeout. SQL Server and Oracle can take a long time, especially on first image pull.

The four-driver scope includes SELECT compatibility and int64 transport, INSERT/UPDATE/DELETE through Gateway REST and MCP, driver-native affected counts including zero rows, constraint-to-409 normalization with rollback and persisted-state checks, agent-policy timeout rollback, gateway-timeout cancellation/state verification, startup DML EXPLAIN and runtime least-privilege failures, SQL lint bypass resistance, output limits, private driver errors, and unreachable-DB startup failure. Broader Gateway authentication, reconnect, OpenAPI, Docker packaging, and process-lifecycle depth remains in the PostgreSQL and operational suites.

```bash
make test-it-docker-ops
```

Runs the Docker image build/run and Docker Compose env preservation tests. Use this when changing `Dockerfile`, `docker-compose.yml`, examples, env handling, or release packaging behavior.

## DB Selection

`make test-it` defaults to PostgreSQL. Select another DB with a Go test argument:

```bash
go test -tags=integration ./it/... -args -onprest-it-db=mysql
go test -tags=integration ./it/... -args -onprest-it-db=sqlserver
go test -tags=integration ./it/... -args -onprest-it-db=oracle
go test -tags=integration ./it/... -args -onprest-it-db=all
```

Supported values are `postgres`, `mysql`, `sqlserver`, `oracle`, and `all`. `postgresql` is accepted as an alias for `postgres`.

## Normal Coverage

Normal `make test-it` covers:

- PostgreSQL integration path
- PostgreSQL testcontainer startup
- YAML-rendered DB connection details
- gateway/agent binary startup in the main PostgreSQL flow
- REST, MCP, OpenAPI, and WebSocket behavior covered by the PostgreSQL suite

Normal `make test-it` does not cover the MySQL, SQL Server, or Oracle subtests under the `TestContainerDBDriver*` matrix, including their mutation, constraint, timeout/cancel, and permission paths. Those run under `make test-it-all-db`.

It also does not cover:

- `TestDockerTargetsBuildWhenDockerIntegrationEnabled`
- `TestDockerComposeEnvFilePreservesGatewayAPIKeysJSON`

## Release Gate Details

`make test-it-release-gate` runs:

- `govulncheck ./...` using the scanner version pinned in `go.mod`
- `go test ./...`
- PostgreSQL integration suite, three consecutive runs, no skips
- PostgreSQL DB interruption stability, five consecutive runs, no skips
- all-DB container smoke tests, no skips
- Docker image operational test, no skips
- Docker Compose operational test, no skips
- testcontainers leftover check

The release gate reads Go test JSON output and fails if any test emits a skip event. Logs are written to a temporary directory by default. Set `ONPREST_IT_GATE_KEEP_LOGS=1` to keep those logs, or set `ONPREST_IT_GATE_LOG_DIR=/path/to/logs` to choose the log directory.

## Skip Policy

If Docker is not available, DB-backed tests skip by default during ad hoc runs. Set `ONPREST_IT_REQUIRE_CONTAINERS=1` when container-backed DB tests must fail instead of skip. Release validation uses strict mode and treats any skip event as a failure.
