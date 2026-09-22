#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C
export COPYFILE_DISABLE=1

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
source "$ROOT_DIR/scripts/release_targets.sh"

VERSION=${VERSION:?VERSION is required}
RELEASE_TAG=${RELEASE_TAG:?RELEASE_TAG is required}
RELEASE_SHA=${RELEASE_SHA:?RELEASE_SHA is required}
RELEASE_READY_URL=${RELEASE_READY_URL:?RELEASE_READY_URL is required}
RELEASE_DIR=${RELEASE_DIR:-release-dist}

if [[ ! $VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "VERSION must have X.Y.Z form" >&2
	exit 1
fi
if [[ $RELEASE_TAG != "v$VERSION" ]]; then
	echo "RELEASE_TAG must equal vVERSION" >&2
	exit 1
fi
if [[ ! $RELEASE_SHA =~ ^[0-9a-f]{40}$ ]]; then
	echo "RELEASE_SHA must be a full lowercase commit SHA" >&2
	exit 1
fi
if [[ $(git rev-parse HEAD) != "$RELEASE_SHA" ]]; then
	echo "RELEASE_SHA does not match the checked out commit" >&2
	exit 1
fi
if [[ ! $RELEASE_READY_URL =~ ^https://github\.com/viewlegacy/onprest/actions/runs/[0-9]+$ ]]; then
	echo "RELEASE_READY_URL must identify an onprest Actions run" >&2
	exit 1
fi
if [[ -e $RELEASE_DIR ]] && [[ -n $(find "$RELEASE_DIR" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
	echo "RELEASE_DIR must be absent or empty: $RELEASE_DIR" >&2
	exit 1
fi

mkdir -p "$RELEASE_DIR"
release_asset_dir=$(cd "$RELEASE_DIR" && pwd)
build_dir=$(mktemp -d)
stage_dir=$(mktemp -d)
cleanup() {
	rm -rf "$build_dir" "$stage_dir"
}
trap cleanup EXIT

DIST_DIR=$build_dir VERSION=$VERSION bash scripts/cross_build.sh

dependencies="$RELEASE_DIR/DEPENDENCIES.txt"
{
	echo "Onprest release dependencies"
	echo "release_tag=$RELEASE_TAG"
	echo "commit_sha=$RELEASE_SHA"
} >"$dependencies"

for target in "${ONPREST_RELEASE_TARGETS[@]}"; do
	target_name=${target//\//-}
	ext=""
	archive_ext="tar.gz"
	if [[ $target_name == windows-* ]]; then
		ext=".exe"
		archive_ext="zip"
	fi
	root_name="onprest-$VERSION-$target_name"
	package_dir="$stage_dir/$root_name"
	mkdir -p "$package_dir/dependencies"
	for binary in gateway agent; do
		binary_name="onprest-$binary$ext"
		binary_path="$build_dir/$target_name/$binary_name"
		if [[ ! -s $binary_path ]]; then
			echo "missing release binary: $binary_path" >&2
			exit 1
		fi
		cp "$binary_path" "$package_dir/$binary_name"
		go version -m "$binary_path" >"$package_dir/dependencies/onprest-$binary.txt"
		if ! LC_ALL=C grep -aFq "onprest-release-version:$VERSION" "$binary_path"; then
			echo "embedded version evidence missing from $target_name/$binary_name" >&2
			exit 1
		fi
		{
			echo
			echo "## $target_name/$binary_name"
			cat "$package_dir/dependencies/onprest-$binary.txt"
		} >>"$dependencies"
	done
	cp LICENSE "$package_dir/LICENSE"
	cp release/gateway.env.example "$package_dir/gateway.env.example"
	cp release/capability.yaml.example "$package_dir/capability.yaml.example"
	cp release/INSTALL.md "$package_dir/INSTALL.md"
	cat >"$package_dir/RELEASE-MANIFEST.txt" <<EOF
version=$VERSION
release_tag=$RELEASE_TAG
commit_sha=$RELEASE_SHA
target=$target_name
gateway=onprest-gateway$ext
agent=onprest-agent$ext
release_ready_url=$RELEASE_READY_URL
EOF
	if [[ $archive_ext == zip ]]; then
		(cd "$stage_dir" && zip -q -r "$release_asset_dir/$root_name.zip" "$root_name")
	else
		tar -C "$stage_dir" -czf "$RELEASE_DIR/$root_name.tar.gz" "$root_name"
	fi
	rm -rf "$package_dir"
done

cat >"$RELEASE_DIR/RELEASE-EVIDENCE.txt" <<EOF
release_tag=$RELEASE_TAG
version=$VERSION
commit_sha=$RELEASE_SHA
release_ready_url=$RELEASE_READY_URL
release_workflow=viewlegacy/onprest/.github/workflows/release.yml
release_ref=refs/tags/$RELEASE_TAG
EOF

(
	cd "$RELEASE_DIR"
	archives=()
	for target in "${ONPREST_RELEASE_TARGETS[@]}"; do
		target_name=${target//\//-}
		ext="tar.gz"
		if [[ $target_name == windows-* ]]; then ext="zip"; fi
		archives+=("onprest-$VERSION-$target_name.$ext")
	done
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "${archives[@]}" >.archive-digests
	else
		shasum -a 256 "${archives[@]}" >.archive-digests
	fi
)
