package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestAgentCLIPrintsVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var stdout, stderr bytes.Buffer
		handled, code := HandleCLI(context.Background(), []string{arg}, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("HandleCLI(%q) handled=%t code=%d", arg, handled, code)
		}
		if got := strings.TrimSpace(stdout.String()); got != "dev" {
			t.Fatalf("HandleCLI(%q) stdout=%q, want dev", arg, got)
		}
		if stderr.Len() != 0 {
			t.Fatalf("HandleCLI(%q) stderr=%q, want empty", arg, stderr.String())
		}
	}
}

func TestAgentCLIHelpListsVersionAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code := HandleCLI(context.Background(), []string{"help"}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("HandleCLI(help) handled=%t code=%d", handled, code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("help stderr=%q, want empty", stderr.String())
	}
	for _, alias := range []string{"onprest-agent version", "onprest-agent --version", "onprest-agent -v"} {
		if !strings.Contains(stdout.String(), alias) {
			t.Fatalf("help=%q missing %q", stdout.String(), alias)
		}
	}
}
