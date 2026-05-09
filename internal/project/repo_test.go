package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileBuildsSelectableSingleBinaryTargets(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(b)
	for _, want := range []string{
		"ARG TARGET=gateway",
		"go build -trimpath -ldflags=\"-s -w\" -o /out/onprest ./cmd/${TARGET}",
		"ENTRYPOINT [\"/app/onprest\"]",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile missing %q", want)
		}
	}
}

func TestExamplesIncludeCurrentPublicConfigurationFields(t *testing.T) {
	root := repoRoot(t)
	gatewayEnv := readText(t, filepath.Join(root, "examples", "gateway.env"))
	for _, want := range []string{
		"GATEWAY_ADDR=:8080",
		"GATEWAY_AGENT_PUBLIC_KEY=",
		"GATEWAY_API_KEYS_JSON=",
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=10",
		"GATEWAY_RATE_LIMIT_BURST=20",
	} {
		if !strings.Contains(gatewayEnv, want) {
			t.Fatalf("examples/gateway.env missing %q", want)
		}
	}
	if !strings.Contains(gatewayEnv, `GATEWAY_API_KEYS_JSON='[`) {
		t.Fatalf("examples/gateway.env must single-quote GATEWAY_API_KEYS_JSON to preserve bcrypt dollar signs")
	}

	capabilityYAML := readText(t, filepath.Join(root, "examples", "capability.postgres.yaml"))
	for _, want := range []string{
		"driver: postgres",
		"max_size: 10MB",
		"max_files: 3",
		"readonly: true",
		"expose_in_openapi: true",
	} {
		if !strings.Contains(capabilityYAML, want) {
			t.Fatalf("examples/capability.postgres.yaml missing %q", want)
		}
	}
}

func TestRepositoryDoesNotAddCaddyImplementationDependency(t *testing.T) {
	root := repoRoot(t)
	allowed := map[string]bool{
		filepath.Join(root, "architecture.md"):               true,
		filepath.Join(root, "architecture-test-plan.md"):     true,
		filepath.Join(root, "README.md"):                     true,
		filepath.Join(root, "AGENTS.md"):                     true,
		filepath.Join(root, "internal/project/repo_test.go"): true,
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dist", "docs", "it", "local":
				return filepath.SkipDir
			}
			return nil
		}
		if allowed[path] {
			return nil
		}
		name := strings.ToLower(d.Name())
		if strings.Contains(name, "caddy") {
			t.Fatalf("Caddy-specific implementation artifact found: %s", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(b)), "caddy") {
			t.Fatalf("Caddy-specific implementation reference found in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
}
