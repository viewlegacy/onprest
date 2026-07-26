.PHONY: build build-cross quickstart-db quickstart-db-down test test-it test-it-postgres-ci test-it-postgres-stability test-it-all-db test-it-docker-ops test-it-release-gate fmt vet clean

DIST_DIR ?= dist

build:
	mkdir -p "$(DIST_DIR)"
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$(DIST_DIR)/onprest-gateway" ./cmd/gateway
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$(DIST_DIR)/onprest-agent" ./cmd/agent

build-cross:
	bash scripts/cross_build.sh

quickstart-db:
	docker compose -f examples/postgres.compose.yml up -d

quickstart-db-down:
	docker compose -f examples/postgres.compose.yml down -v --remove-orphans

test:
	go test ./...

test-it:
	go test -tags=integration ./it/...

test-it-postgres-ci:
	ONPREST_IT_REQUIRE_CONTAINERS=1 go test -tags=integration ./it/... -skip '^TestDocker' -count=3 -args -onprest-it-db=postgres

test-it-postgres-stability:
	ONPREST_IT_REQUIRE_CONTAINERS=1 go test -tags=integration ./it/... -run '^TestPostgresDBUnreachableDuringQuery$$' -count=5 -v -args -onprest-it-db=postgres

test-it-all-db:
	ONPREST_IT_REQUIRE_CONTAINERS=1 go test -tags=integration ./it/... -run '^TestContainerDBDriver' -timeout 30m -count=1 -args -onprest-it-db=all

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
