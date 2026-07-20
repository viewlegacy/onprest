package project

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMakeBuildProducesOnlyRunnableGatewayAndAgent(t *testing.T) {
	root := repoRoot(t)
	dist := t.TempDir()
	runRepoCommand(t, root, []string{"DIST_DIR=" + dist}, "make", "build")
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("make build entries = %v, want exactly two binaries", entryNames(entries))
	}
	for _, name := range []string{"onprest-gateway", "onprest-agent"} {
		path := filepath.Join(dist, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		if info.Mode()&0o111 == 0 {
			t.Fatalf("%s is not executable: %v", name, info.Mode())
		}
		runRepoCommand(t, root, nil, path, "--help")
	}
	packages := runRepoCommand(t, root, nil, "go", "list", "./...")
	if strings.Contains(strings.ToLower(packages), "dashboard") || strings.Contains(packages, "/manage") {
		t.Fatalf("OSS package list contains managed/dashboard code:\n%s", packages)
	}
}

func TestMakeBuildCrossProducesBothBinariesForEveryTarget(t *testing.T) {
	root := repoRoot(t)
	dist := t.TempDir()
	runRepoCommand(t, root, []string{"DIST_DIR=" + dist}, "make", "build-cross")
	targets := []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64"}
	for _, target := range targets {
		ext := ""
		if strings.HasPrefix(target, "windows-") {
			ext = ".exe"
		}
		entries, err := os.ReadDir(filepath.Join(dist, target))
		if err != nil {
			t.Fatalf("target %s: %v", target, err)
		}
		if len(entries) != 2 {
			t.Fatalf("target %s entries = %v, want two", target, entryNames(entries))
		}
		for _, binary := range []string{"onprest-gateway" + ext, "onprest-agent" + ext} {
			info, err := os.Stat(filepath.Join(dist, target, binary))
			if err != nil || info.Size() == 0 {
				t.Fatalf("target %s binary %s missing/empty: info=%v err=%v", target, binary, info, err)
			}
		}
	}
	nativeTarget := runtime.GOOS + "-" + runtime.GOARCH
	if nativeTarget != "windows-amd64" {
		for _, binary := range []string{"onprest-gateway", "onprest-agent"} {
			if info, err := os.Stat(filepath.Join(dist, nativeTarget, binary)); err == nil && info.Mode()&0o111 == 0 {
				t.Fatalf("native cross-built %s is not executable", binary)
			}
		}
	}
}

func runRepoCommand(t *testing.T, dir string, extraEnv []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output.String())
	}
	return output.String()
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = fmt.Sprintf("%s (%s)", entry.Name(), entry.Type())
	}
	return names
}

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
		"name: legacy",
		"user: readonly_user",
		"password: onprest-example-password",
		"sql: select id, name, email from customers where id = :customer_id",
		"max_size: 10MB",
		"max_files: 3",
		"readonly: true",
		"expose_in_openapi: true",
	} {
		if !strings.Contains(capabilityYAML, want) {
			t.Fatalf("examples/capability.postgres.yaml missing %q", want)
		}
	}

	composeYAML := readText(t, filepath.Join(root, "examples", "postgres.compose.yml"))
	for _, want := range []string{
		"POSTGRES_DB: legacy",
		"POSTGRES_USER: onprest_admin",
		"POSTGRES_PASSWORD: onprest-example-password",
		`"127.0.0.1:5432:5432"`,
		"./postgres-init.sql:/docker-entrypoint-initdb.d/001-onprest-example.sql:ro",
	} {
		if !strings.Contains(composeYAML, want) {
			t.Fatalf("examples/postgres.compose.yml missing %q", want)
		}
	}

	initSQL := readText(t, filepath.Join(root, "examples", "postgres-init.sql"))
	for _, want := range []string{
		"CREATE ROLE readonly_user LOGIN PASSWORD 'onprest-example-password'",
		"CREATE TABLE customers",
		"(1, 'Ada Lovelace', 'ada@example.com')",
		"GRANT SELECT ON customers TO readonly_user",
	} {
		if !strings.Contains(initSQL, want) {
			t.Fatalf("examples/postgres-init.sql missing %q", want)
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
