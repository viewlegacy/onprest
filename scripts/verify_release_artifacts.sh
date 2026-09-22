#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
source "$ROOT_DIR/scripts/release_targets.sh"

VERSION=${VERSION:?VERSION is required}
RELEASE_TAG=${RELEASE_TAG:?RELEASE_TAG is required}
RELEASE_SHA=${RELEASE_SHA:?RELEASE_SHA is required}
RELEASE_READY_URL=${RELEASE_READY_URL:?RELEASE_READY_URL is required}
RELEASE_DIR=${RELEASE_DIR:-release-dist}

go run ./internal/releaseverify \
	"$RELEASE_DIR" "$VERSION" "$RELEASE_TAG" "$RELEASE_SHA" "$RELEASE_READY_URL" \
	"${ONPREST_RELEASE_TARGETS[@]}"
