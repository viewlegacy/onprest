package gateway

import (
	"bytes"
	"strings"
	"testing"
)

func TestGatewayCLIPrintsVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var stdout, stderr bytes.Buffer
		if !HandleCLI([]string{arg}, &stdout, &stderr) {
			t.Fatalf("HandleCLI(%q) not handled", arg)
		}
		if got := strings.TrimSpace(stdout.String()); got != "dev" {
			t.Fatalf("HandleCLI(%q) stdout=%q, want dev", arg, got)
		}
		if stderr.Len() != 0 {
			t.Fatalf("HandleCLI(%q) stderr=%q, want empty", arg, stderr.String())
		}
	}
}

func TestGatewayCLIHelpListsVersionAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if !HandleCLI([]string{"help"}, &stdout, &stderr) {
		t.Fatal("HandleCLI(help) not handled")
	}
	if stderr.Len() != 0 {
		t.Fatalf("help stderr=%q, want empty", stderr.String())
	}
	for _, alias := range []string{"gateway version", "gateway --version", "gateway -v"} {
		if !strings.Contains(stdout.String(), alias) {
			t.Fatalf("help=%q missing %q", stdout.String(), alias)
		}
	}
}
