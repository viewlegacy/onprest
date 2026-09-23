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
evidence="$RELEASE_DIR/VULNERABILITY-EVIDENCE.txt"
archive_digests="$RELEASE_DIR/.archive-digests"
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT
test -s "$archive_digests"

{
	echo "Onprest release vulnerability scans"
	echo "version=$VERSION"
	echo "commit_sha=$RELEASE_SHA"
	echo
	echo "## source ./..."
	go tool govulncheck ./...
} >"$evidence"

for target in "${ONPREST_RELEASE_TARGETS[@]}"; do
	target_name=${target//\//-}
	ext=""
	archive="$RELEASE_DIR/onprest-$target_name.tar.gz"
	if [[ $target_name == windows-* ]]; then
		ext=".exe"
		archive="$RELEASE_DIR/onprest-$target_name.zip"
	fi
	extract_dir="$tmp_dir/$target_name"
	go run ./internal/releaseverify extract \
		"$archive" "$extract_dir" "$VERSION" "$RELEASE_TAG" "$RELEASE_SHA" "$RELEASE_READY_URL" \
		"$target" "$archive_digests"
	for binary in gateway agent; do
		path="$extract_dir/onprest-$VERSION-$target_name/onprest-$binary$ext"
		echo >>"$evidence"
		echo "## $target_name/onprest-$binary$ext" >>"$evidence"
		go tool govulncheck -mode=binary "$path" >>"$evidence"
	done
done

go run ./internal/releaseverify extract \
	"$RELEASE_DIR/onprest-quickstart.tar.gz" "$tmp_dir/quickstart" \
	"$VERSION" "$RELEASE_TAG" "$RELEASE_SHA" "$RELEASE_READY_URL" \
	quickstart "$archive_digests"

rm -f "$archive_digests"
