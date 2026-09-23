#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
source "$ROOT_DIR/scripts/release_targets.sh"
DIST_DIR="${DIST_DIR:-dist}"
VERSION="${VERSION:-dev}"
VERSION_LDFLAGS="-X github.com/viewlegacy/onprest/internal/buildinfo.Version=${VERSION} -X github.com/viewlegacy/onprest/internal/buildinfo.ReleaseMarker=onprest-release-version:${VERSION}"

build_one() {
	local goos="$1"
	local goarch="$2"
	local cmd="$3"
	local name="onprest-$cmd"
	local ext=""
	if [[ "$goos" == "windows" ]]; then
		ext=".exe"
	fi
	local out_dir="$DIST_DIR/$goos-$goarch"
	mkdir -p "$out_dir"
	echo "==> $goos/$goarch $name"
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
		go build -buildvcs=false -trimpath -ldflags="$VERSION_LDFLAGS" -o "$out_dir/$name$ext" "./cmd/$cmd"
}

for target in "${ONPREST_RELEASE_TARGETS[@]}"; do
	IFS=/ read -r goos goarch <<<"$target"
	build_one "$goos" "$goarch" gateway
	build_one "$goos" "$goarch" agent
done
