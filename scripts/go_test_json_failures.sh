#!/usr/bin/env bash
# Print each failed test of a `go test -json` log with its own output.
set -euo pipefail

log=${1:?go test -json log is required}
max_lines=${2:-60}

failed_packages=$(grep -E '"Action":"fail"' "$log" | grep -v '"Test":' || true)
failed_tests=$(grep -E '"Action":"fail".*"Test":"[^"]+"' "$log" || true)
if [[ -z $failed_tests ]]; then
	echo "no failed test records in $log; package-level failures:" >&2
	if [[ -n $failed_packages ]]; then
		printf '%s\n' "$failed_packages" >&2
	fi
	exit 0
fi
names=$(printf '%s\n' "$failed_tests" | sed -E 's/.*"Package":"([^"]*)".*"Test":"(([^"\\]|\\.)*)".*/\1	\2/')
count=$(printf '%s\n' "$names" | wc -l | tr -d ' ')
echo "failed tests ($count, each with at most $max_lines output lines, subtests before their parents):" >&2
printf '%s\n' "$names" | sed -E 's/^([^	]*)	(.*)$/--- FAIL: \1 \2/; s/\\"/"/g' >&2
printf '%s\n' "$names" | awk -F'\t' '
	{ line[NR] = $0; pkg[NR] = $1; name[NR] = $2 }
	END {
		for (pass = 0; pass < 2; pass++)
			for (i = 1; i <= NR; i++) {
				parent = 0
				for (j = 1; j <= NR; j++)
					if (j != i && pkg[j] == pkg[i] && index(name[j], name[i] "/") == 1) { parent = 1; break }
				if (parent == pass) print line[i]
			}
	}' | while IFS=$'\t' read -r package test; do
	echo "=== output: $package ${test//\\\"/\"}" >&2
	grep -F "\"Package\":\"$package\",\"Test\":\"$test\",\"Output\":" "$log" |
		sed -E 's/.*"Output":"(.*)"}$/\1/; s/\\n$//; s/\\"/"/g' |
		head -n "$max_lines" >&2 || true
done
if [[ -n $failed_packages ]]; then
	echo "failed packages:" >&2
	printf '%s\n' "$failed_packages" | sed -E 's/.*"Package":"([^"]*)".*/FAIL \1/' >&2
fi
