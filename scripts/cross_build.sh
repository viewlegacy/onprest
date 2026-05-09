#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

TARGETS=(
	"linux/amd64"
	"linux/arm64"
	"darwin/amd64"
	"darwin/arm64"
	"windows/amd64"
)

build_one() {
	local goos="$1"
	local goarch="$2"
	local cmd="$3"
	local name="onprest-$cmd"
	local ext=""
	if [[ "$goos" == "windows" ]]; then
		ext=".exe"
	fi
	local out_dir="dist/$goos-$goarch"
	mkdir -p "$out_dir"
	echo "==> $goos/$goarch $name"
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
		go build -trimpath -ldflags="-s -w" -o "$out_dir/$name$ext" "./cmd/$cmd"
}

for target in "${TARGETS[@]}"; do
	IFS=/ read -r goos goarch <<<"$target"
	build_one "$goos" "$goarch" gateway
	build_one "$goos" "$goarch" agent
done
