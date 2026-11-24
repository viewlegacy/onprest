# Contributing

Thanks for helping improve Onprest.

You need the Go version declared in `go.mod`. Integration tests also need Docker, and schema validation needs Node.js with npm.

## Build

```sh
make build        # dist/onprest-gateway, dist/onprest-agent
make build-cross  # dist/<os>-<arch>/ for every supported platform
```

Set `VERSION=x.y.z` to embed a version; otherwise the binaries report `dev`. Builds use `CGO_ENABLED=0`.

## Test

```sh
make test   # unit tests: go test -timeout 60m ./...
make vet
gofmt -w cmd internal
make test-it   # integration tests against PostgreSQL
```

Unit tests need no database or Docker. Keep the 60-minute timeout when you run `go test` directly; the Wasm worker tests alone take longer than the default 10 minutes.

Integration tests start real databases with Testcontainers. To target another database:

```sh
go test -tags=integration ./it/... -args -onprest-it-db=mysql   # postgres, mysql, sqlserver, oracle, or all
```

Also run the suite that matches what you changed:

| You changed | Run |
|---|---|
| Database drivers, SQL handling, or mutations | `make test-it-all-db` (all four databases, up to 30 minutes) |
| Database TLS | `make test-it-database-tls` |
| Handling of a database that drops mid-query | `make test-it-postgres-stability` |
| Public OpenAPI or MCP schemas | `make test-it-postgres-ci` |
| `Dockerfile`, Compose, or env-file handling | `make test-it-docker-ops` |
| Dependencies | `make vulncheck` |

`make test-it-release-gate` runs all of these together, as CI does. [`it/README.md`](it/README.md) describes each suite in detail.

By default, container-backed tests are skipped when Docker is unavailable. Set `ONPREST_IT_REQUIRE_CONTAINERS=1` to make that a failure, as CI does. After an interrupted run, remove leftover containers listed by `docker ps -a --filter label=org.testcontainers=true`.

## Pull requests

Keep changes focused. The gateway must remain stateless, and SQL and database credentials must stay on the agent side. Do not add dashboard or managed-service-only code to the OSS core.

Include:

- What changed
- Security impact
- Test coverage or manual verification

Pull requests to `main` run these checks:

| Check | Runs |
|---|---|
| `release-gate / ci / *` | `go test ./...` and `go vet ./...` |
| `release-gate / package-candidate` | Builds the release archives, scans the binaries for vulnerabilities, and verifies checksums |
| `release-gate / integration-linux` | `make test-it-release-gate` with the binaries extracted from the Linux amd64 archive |
| `release-gate / native-tests` | The agent's OS-dependent tests on Linux arm64, macOS, and Windows |
| `service-lifecycle / *` | Installs, starts, stops, and removes the agent under systemd, launchd, and Windows Service |
| `release-gate / docs` | Builds the documentation site |
| `release-gate / checks` | Requires every job above to succeed |

A pull request that changes only documentation runs only `docs`. A skipped test fails `integration-linux` and `native-tests`.

## Releases

Pushing a tag `vX.Y.Z` starts the release workflow. It proceeds only if `release-gate / release-ready` succeeded for the exact tagged commit. The workflow rebuilds the archives from the tag, reruns the Linux integration and service checks against those exact archives, attaches a GitHub artifact attestation to each asset, publishes `SHA256SUMS`, and creates the GitHub Release. It never overwrites an existing release.

Cross-builds do not prove that a binary runs on an older OS. Before a release, verify the candidate on the minimum version of each [supported platform](https://docs.onprest.viewlegacy.com/operations/deployment#supported-platforms).
