package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every eligible test belongs to exactly one shard; exclusions are explicit.
func TestPartitionCoverage(t *testing.T) {
	tests := []testCase{{"TestA", "common_test.go"}, {"TestB", "native_test.go"}, {"TestC", "native_test.go"}, {"TestD", "native_test.go"}}
	seen := map[string]int{}
	for shard := 1; shard <= 2; shard++ {
		for _, name := range partition(tests, regexp.MustCompile("^Test"), regexp.MustCompile("^TestC$"), regexp.MustCompile("native"), shard, 2) {
			seen[name]++
		}
	}
	if len(seen) != 2 || seen["TestB"] != 1 || seen["TestD"] != 1 {
		t.Fatal(seen)
	}
}

func TestRunnerRejectsFailuresSkipsAndEmptyRunsAndKeepsEvidence(t *testing.T) {
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	for _, mode := range []string{"pass", "fail", "skip", "empty"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CITEST_FIXTURE_MODE", mode)
			log := filepath.Join(t.TempDir(), "tests.jsonl")
			args := []string{"test", "-json", "-count=1", "./testdata/fixture"}
			if mode == "empty" {
				args = append(args, "-run=^NoTestsSelected$")
			}
			status := run(log, args)
			if (status == 0) != (mode == "pass") {
				t.Fatalf("%s: status %d", mode, status)
			}
			for _, path := range []string{log, log + ".md"} {
				data, err := os.ReadFile(path)
				if err != nil || len(data) == 0 {
					t.Fatalf("missing evidence %s: %v", path, err)
				}
			}
			if mode == "skip" {
				data, _ := os.ReadFile(log + ".md")
				if !strings.Contains(string(data), "skipped: 1") {
					t.Fatal(string(data))
				}
			}
		})
	}
}
