.PHONY: build build-cross package-release scan-release-binaries finalize-release-artifacts release-artifacts verify-release-artifacts quickstart-db quickstart-db-down reconciliation-db reconciliation-db-down reconciliation-change-order test vulncheck test-it test-it-postgres-ci test-it-postgres-stability test-it-all-db test-it-database-tls test-it-docker-ops test-it-release-gate fmt vet clean

DIST_DIR ?= dist
RELEASE_DIR ?= release-dist
RECONCILIATION_DB ?= postgres
VERSION ?= dev
VERSION_LDFLAGS := -X github.com/viewlegacy/onprest/internal/buildinfo.Version=$(VERSION) -X github.com/viewlegacy/onprest/internal/buildinfo.ReleaseMarker=onprest-release-version:$(VERSION)

build:
	mkdir -p "$(DIST_DIR)"
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="$(VERSION_LDFLAGS)" -o "$(DIST_DIR)/onprest-gateway" ./cmd/gateway
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="$(VERSION_LDFLAGS)" -o "$(DIST_DIR)/onprest-agent" ./cmd/agent

build-cross:
	bash scripts/cross_build.sh

package-release:
	bash scripts/package_release.sh

scan-release-binaries:
	bash scripts/scan_release_binaries.sh

finalize-release-artifacts:
	bash scripts/finalize_release_artifacts.sh

release-artifacts:
	$(MAKE) package-release
	$(MAKE) scan-release-binaries
	$(MAKE) finalize-release-artifacts
	$(MAKE) verify-release-artifacts

verify-release-artifacts:
	bash scripts/verify_release_artifacts.sh

quickstart-db:
	docker compose -f examples/postgres.compose.yml up -d

quickstart-db-down:
	docker compose -f examples/postgres.compose.yml down -v --remove-orphans

reconciliation-db:
	bash examples/mutation-reconciliation/db.sh up "$(RECONCILIATION_DB)"

reconciliation-db-down:
	bash examples/mutation-reconciliation/db.sh down "$(RECONCILIATION_DB)"

reconciliation-change-order:
	bash examples/mutation-reconciliation/db.sh change-order "$(RECONCILIATION_DB)"

test:
	go test ./...

vulncheck:
	go tool govulncheck ./...

test-it:
	go test -tags=integration ./it/...

test-it-postgres-ci:
	ONPREST_IT_REQUIRE_CONTAINERS=1 go test -tags=integration ./it/... -skip '^TestDocker' -count=3 -args -onprest-it-db=postgres

test-it-postgres-stability:
	ONPREST_IT_REQUIRE_CONTAINERS=1 go test -tags=integration ./it/... -run '^TestPostgresDBUnreachableDuringQuery$$' -count=5 -v -args -onprest-it-db=postgres

test-it-all-db:
	ONPREST_IT_REQUIRE_CONTAINERS=1 go test -tags=integration ./it/... -run '^TestContainerDBDriver' -timeout 30m -count=1 -args -onprest-it-db=all

test-it-database-tls:
	ONPREST_IT_REQUIRE_CONTAINERS=1 go test -tags=integration ./it/... -run '^(TestPostgresTLSModesPrivateCAClientCertificateAndHostnameVerification|TestMySQLTLSModesClientCertificateRotationAndReconnectAgainstRealDatabase|TestSQLServerTLSRequireAndVerifyFullAgainstRealDatabase|TestOracleTLSModesVerificationRotationAndReconnectAgainstRealDatabase)$$' -timeout 30m -count=1 -args -onprest-it-db=all

test-it-docker-ops:
	ONPREST_IT_DOCKER=1 go test -tags=integration ./it/... -run '^TestDockerTargetsBuildWhenDockerIntegrationEnabled$$' -count=1 -v
	ONPREST_IT_DOCKER_COMPOSE=1 go test -tags=integration ./it/... -run '^TestDockerComposeEnvFilePreservesGatewayAPIKeysJSON$$' -count=1 -v

test-it-release-gate:
	bash scripts/it_release_gate.sh

fmt:
	gofmt -w cmd internal it

vet:
	go vet ./...

clean:
	rm -rf "$(DIST_DIR)"
