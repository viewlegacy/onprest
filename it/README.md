# Integration Tests

Integration tests are build-tagged and do not run with the default unit test command.

DB-backed integration tests always start temporary DB containers with testcontainers, render the container connection details into the test capability YAML, and then run the agent against that YAML. Self-hosted DB overrides through `ONPREST_IT_POSTGRES_*`, `ONPREST_IT_MYSQL_*`, `ONPREST_IT_SQLSERVER_*`, `ONPREST_IT_ORACLE_*`, or `ONPREST_IT_POSTGRES_READONLY_*` are intentionally not supported.

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

Final OSS core release gate. This runs unit tests, PostgreSQL CI, PostgreSQL DB interruption stability, all-DB smoke, Docker image operations, Docker Compose operations, skip detection, and testcontainers leftover detection.

## Support Commands

Use these when touching a specific area.

```bash
make test-it-postgres-stability
```

Runs `TestPostgresDBUnreachableDuringQuery` five times. Use this when changing DB interruption, DB reachability, or error classification behavior. The test waits until PostgreSQL reports the long-running query as active before stopping the container.

```bash
make test-it-all-db
```

Runs the container-backed DB smoke tests for PostgreSQL, MySQL, SQL Server, and Oracle with `ONPREST_IT_REQUIRE_CONTAINERS=1` and a longer Go test timeout. The smoke path creates and seeds `onprest_it_customers`, renders the selected container connection into capability YAML, starts the agent, and verifies the REST result. SQL Server and Oracle can take a long time, especially on first image pull.

The all-DB gate focuses on DB-dependent behavior: DSN generation, driver startup, placeholder conversion, DB-specific SQL syntax, EXPLAIN/lint startup, schema/seed/result scanning, max row limiting, max byte limiting, driver error hiding, and startup failure when the configured DB is unreachable. Gateway, WebSocket, MCP, OpenAPI, auth, reconnect, and process lifecycle depth remains covered by the PostgreSQL integration suite.

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

Normal `make test-it` does not cover:

- `TestContainerDBDriverSmoke/mysql`
- `TestContainerDBDriverSmoke/sqlserver`
- `TestContainerDBDriverSmoke/oracle`
- `TestContainerDBDriverErrorsAreHiddenFromGatewayResponse/mysql`
- `TestContainerDBDriverErrorsAreHiddenFromGatewayResponse/sqlserver`
- `TestContainerDBDriverErrorsAreHiddenFromGatewayResponse/oracle`
- `TestDockerTargetsBuildWhenDockerIntegrationEnabled`
- `TestDockerComposeEnvFilePreservesGatewayAPIKeysJSON`

## Release Gate Details

`make test-it-release-gate` runs:

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
