#!/usr/bin/env bash
set -euo pipefail

input=${1:?release version is required}
version=${input#v}
if [[ ! $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "release version must be X.Y.Z or vX.Y.Z" >&2
	exit 1
fi
printf '%s\n' "$version"
