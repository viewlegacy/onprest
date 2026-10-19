#!/usr/bin/env bash
# Print each failed test of a `go test -json` log with its own output.
set -euo pipefail

log=${1:?go test -json log is required}
max_tests=${2:-40}
max_lines=${3:-60}

failed_packages=$(grep -E '"Action":"fail"' "$log" | grep -v '"Test":' || true)
failed_tests=$(grep -E '"Action":"fail".*"Test":"[^"]+"' "$log" || true)
if [[ -z $failed_tests ]]; then
	echo "no failed test records in $log; package-level failures:" >&2
	if [[ -n $failed_packages ]]; then
		printf '%s\n' "$failed_packages" >&2
	fi
	exit 0
fi
count=$(printf '%s\n' "$failed_tests" | wc -l | tr -d ' ')
echo "failed tests ($count, showing at most $max_tests with $max_lines output lines each):" >&2
printf '%s\n' "$failed_tests" | sed -E 's/.*"Package":"([^"]*)".*"Test":"([^"]*)".*/--- FAIL: \1 \2/' >&2
printf '%s\n' "$failed_tests" | sed -n "1,${max_tests}p" | while IFS= read -r record; do
	package=$(sed -E 's/.*"Package":"([^"]*)".*/\1/' <<<"$record")
	test=$(sed -E 's/.*"Test":"([^"]*)".*/\1/' <<<"$record")
	echo "=== output: $package $test" >&2
	grep -F "\"Package\":\"$package\",\"Test\":\"$test\",\"Output\":" "$log" |
		sed -E 's/.*"Output":"(.*)"}$/\1/; s/\\n$//; s/\\"/"/g' |
		head -n "$max_lines" >&2 || true
done
if [[ -n $failed_packages ]]; then
	echo "failed packages:" >&2
	printf '%s\n' "$failed_packages" | sed -E 's/.*"Package":"([^"]*)".*/FAIL \1/' >&2
fi
