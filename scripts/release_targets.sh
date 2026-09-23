#!/usr/bin/env bash

# The public archive matrix has one source of truth. Consumers use the
# slash-separated values for GOOS/GOARCH and replace / with - for asset names.
ONPREST_RELEASE_TARGETS=(
	"linux/amd64"
	"linux/arm64"
	"darwin/amd64"
	"darwin/arm64"
	"windows/amd64"
)
