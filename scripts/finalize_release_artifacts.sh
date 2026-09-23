#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
source "$ROOT_DIR/scripts/release_targets.sh"

VERSION=${VERSION:?VERSION is required}
RELEASE_DIR=${RELEASE_DIR:-release-dist}
if [[ -e "$RELEASE_DIR/.archive-digests" ]]; then
	echo "intermediate archive digests were not consumed by the safe scanner" >&2
	exit 1
fi

publish_alias() {
	local canonical=$1 alias=$2 temp
	temp=$(mktemp "$RELEASE_DIR/.onprest-alias.XXXXXX")
	cp "$canonical" "$temp"
	mv -f "$temp" "$alias"
}

for target in "${ONPREST_RELEASE_TARGETS[@]}"; do
	target_name=${target//\//-}
	ext="tar.gz"
	if [[ $target_name == windows-* ]]; then ext="zip"; fi
	test -s "$RELEASE_DIR/onprest-$VERSION-$target_name.$ext"
	publish_alias "$RELEASE_DIR/onprest-$VERSION-$target_name.$ext" "$RELEASE_DIR/onprest-$target_name.$ext"
done
test -s "$RELEASE_DIR/onprest-$VERSION-quickstart.tar.gz"
publish_alias "$RELEASE_DIR/onprest-$VERSION-quickstart.tar.gz" "$RELEASE_DIR/onprest-quickstart.tar.gz"
for evidence in DEPENDENCIES.txt RELEASE-EVIDENCE.txt VULNERABILITY-EVIDENCE.txt; do
	test -s "$RELEASE_DIR/$evidence"
done

(
	cd "$RELEASE_DIR"
	files=()
	while IFS= read -r file; do files+=("$file"); done < <(find . -maxdepth 1 -type f ! -name SHA256SUMS -print | sed 's#^./##' | LC_ALL=C sort)
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "${files[@]}" >SHA256SUMS
	else
		shasum -a 256 "${files[@]}" >SHA256SUMS
	fi
)
