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

	"gopkg.in/yaml.v3"
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
	if !strings.Contains(gatewayEnv, `"capabilities":["get_customer","update_customer"]`) || strings.Contains(gatewayEnv, `"capabilities":["*"]`) {
		t.Fatal("examples/gateway.env must use the explicit Quick Start capability allow-list")
	}

	capabilityYAML := readText(t, filepath.Join(root, "examples", "capability.postgres.yaml"))
	for _, want := range []string{
		"driver: postgres",
		"name: legacy",
		"user: capability_user",
		"password: onprest-example-password",
		"sql: select id, name, email from customers where id = :customer_id",
		"sql: update customers set name = :name where id = :customer_id",
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
		"CREATE ROLE capability_user LOGIN PASSWORD 'onprest-example-password'",
		"CREATE TABLE customers",
		"(1, 'Ada Lovelace', 'ada@example.com')",
		"GRANT SELECT ON customers TO capability_user",
		"GRANT UPDATE (name) ON customers TO capability_user",
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

func TestGitHubActionsSeparateFastAndMainReleaseChecks(t *testing.T) {
	root := repoRoot(t)
	workflowDir := filepath.Join(root, ".github", "workflows")

	ci := readWorkflow(t, filepath.Join(workflowDir, "ci.yml"))
	ciEvents := workflowSection(t, ci, "on")
	for _, event := range []string{"push", "pull_request"} {
		if _, ok := ciEvents[event]; !ok {
			t.Fatalf("ci workflow missing %s trigger", event)
		}
	}
	ciText := readText(t, filepath.Join(workflowDir, "ci.yml"))
	for _, command := range []string{"go test ./...", "go vet ./..."} {
		if !strings.Contains(ciText, command) {
			t.Fatalf("ci workflow missing %q", command)
		}
	}

	release := readWorkflow(t, filepath.Join(workflowDir, "release-gate.yml"))
	assertMainWorkflowTriggers(t, release)
	releaseJobs := workflowSection(t, release, "jobs")
	for _, job := range []string{"integration-linux", "service-lifecycle", "release-ready"} {
		if _, ok := releaseJobs[job]; !ok {
			t.Fatalf("release gate workflow missing %s job", job)
		}
	}
	if text := readText(t, filepath.Join(workflowDir, "release-gate.yml")); !strings.Contains(text, "make test-it-release-gate") ||
		!strings.Contains(text, "uses: ./.github/workflows/service-lifecycle.yml") ||
		!strings.Contains(text, "SERVICE_RESULT") || !strings.Contains(text, "concurrency:") || !strings.Contains(text, "cancel-in-progress: true") {
		t.Fatal("release gate workflow does not aggregate Linux integration and reusable service lifecycle")
	}

	service := readWorkflow(t, filepath.Join(workflowDir, "service-lifecycle.yml"))
	serviceEvents := workflowSection(t, service, "on")
	for _, event := range []string{"workflow_call", "workflow_dispatch"} {
		if _, ok := serviceEvents[event]; !ok {
			t.Fatalf("service lifecycle workflow missing %s trigger", event)
		}
	}
	for _, event := range []string{"push", "pull_request"} {
		if _, ok := serviceEvents[event]; ok {
			t.Fatalf("service lifecycle workflow must be triggered through release-gate, found direct %s", event)
		}
	}
	serviceJobs := workflowSection(t, service, "jobs")
	for _, job := range []string{"linux-systemd", "macos-launchd", "windows-service"} {
		if _, ok := serviceJobs[job]; !ok {
			t.Fatalf("service lifecycle workflow missing %s job", job)
		}
	}
	serviceText := readText(t, filepath.Join(workflowDir, "service-lifecycle.yml"))
	if strings.Contains(serviceText, "concurrency:") {
		t.Fatal("called service lifecycle workflow must not share caller concurrency and cancel its own release gate")
	}
	if strings.Contains(serviceText, "\n    paths:") {
		t.Fatal("service lifecycle must run unconditionally for main PRs and pushes")
	}
	if !strings.Contains(serviceText, "scripts/service-test-systemd.Dockerfile") {
		t.Fatal("linux service lifecycle does not build the systemd test image")
	}
	if strings.Count(serviceText, "TestValidateLatestLogCrashRecoveryProcess") != 3 {
		t.Fatal("service lifecycle must run validate crash recovery on Linux, macOS, and Windows")
	}
	for _, testName := range []string{
		"TestValidationRecoveryPreservesUnknownAndSpecialTemporaryPaths",
		"TestValidationRejectsInvalidFixedPathsWithoutMutation",
	} {
		if strings.Count(serviceText, testName) != 3 {
			t.Fatalf("service lifecycle must run %s on Linux, macOS, and Windows", testName)
		}
	}
	for path, required := range map[string][]string{
		"scripts/test_service_lifecycle_unix.sh":     {"gateway_bin", "kill -0 \"$gateway_pid\"", "runtime_marker_a", "runtime_marker_b", "rollout_marker_new", "assert_old_public_contract", "assert_new_capability_absent", "runtime_writer_pid", "capability.validate-blocking.yaml", "cleanup\ntrap - EXIT"},
		"scripts/test_service_lifecycle_windows.ps1": {"GatewayBin", "gatewayProcess.WaitForExit()", "SetEnvironmentVariable('GATEWAY_API_KEYS_JSON'", "runtime_marker_a", "runtime_marker_b", "rollout_marker_new", "Assert-OldPublicContract", "Assert-NewCapabilityAbsent", "runtimeWriter", "OnprestValidateReader", "temporaryLog"},
	} {
		text := readText(t, filepath.Join(root, filepath.FromSlash(path)))
		for _, marker := range required {
			if !strings.Contains(text, marker) {
				t.Fatalf("%s does not exercise production runtime rotation/validation isolation marker %q", path, marker)
			}
		}
	}
	unixLifecycle := readText(t, filepath.Join(root, "scripts", "test_service_lifecycle_unix.sh"))
	trapAt := strings.Index(unixLifecycle, "trap cleanup EXIT")
	installAt := strings.Index(unixLifecycle, `"${elevate[@]}" "$agent_bin" service install`)
	if trapAt < 0 || installAt <= trapAt || strings.Contains(unixLifecycle[trapAt:installAt], "\ncleanup\n") {
		t.Fatal("Unix lifecycle invokes final cleanup after starting the Gateway but before installing the service")
	}
	for _, invocation := range []string{
		"test_service_lifecycle_unix.sh /workspace/dist/onprest-agent /workspace/dist/onprest-gateway",
		"test_service_lifecycle_unix.sh ./dist/onprest-agent ./dist/onprest-gateway",
		"test_service_lifecycle_windows.ps1 -AgentBin .\\dist\\onprest-agent.exe -GatewayBin .\\dist\\onprest-gateway.exe",
	} {
		if !strings.Contains(serviceText, invocation) {
			t.Fatalf("service lifecycle workflow does not pass both production binaries: %q", invocation)
		}
	}
	systemdImage := readText(t, filepath.Join(root, "scripts", "service-test-systemd.Dockerfile"))
	for _, want := range []string{"systemd-sysv", "postgresql", `CMD ["/sbin/init"]`} {
		if !strings.Contains(systemdImage, want) {
			t.Fatalf("systemd service test image missing %q", want)
		}
	}
}

func TestDatabaseGateDocumentationMatchesExecutableSelection(t *testing.T) {
	root := repoRoot(t)
	makefile := readText(t, filepath.Join(root, "Makefile"))
	releaseScript := readText(t, filepath.Join(root, "scripts", "it_release_gate.sh"))
	integrationReadme := readText(t, filepath.Join(root, "it", "README.md"))
	testCommands := readText(t, filepath.Join(root, "docs", "app", "reference", "test-commands", "page.mdx"))
	releaseDocs := readText(t, filepath.Join(root, "docs", "app", "operations", "release-gate", "page.mdx"))

	const allDBSelector = "-run '^TestContainerDBDriver'"
	for source, text := range map[string]string{
		"Makefile": makefile, "release script": releaseScript, "integration README": integrationReadme, "test commands": testCommands,
	} {
		if !strings.Contains(text, allDBSelector) {
			t.Fatalf("%s does not use common all-DB selection %q", source, allDBSelector)
		}
	}
	for path, name := range map[string]string{
		"it/transaction_start_conformance_test.go": "TestContainerDBDriverOracleTransactionStartIsImmediateAndRollbackable",
		"it/openapi_nullable_postgres_test.go":     "TestContainerDBDriverPostgresNullableResultMatchesGeneratedOpenAPI",
		"it/timestamp_postgres_test.go":            "TestContainerDBDriverPostgresTimestampResultPreservesJSONContract",
	} {
		if text := readText(t, filepath.Join(root, filepath.FromSlash(path))); !strings.Contains(text, "func "+name+"(") {
			t.Fatalf("%s does not use common all-DB test prefix: %s", path, name)
		}
	}
	const postgresTLS = "TestPostgresTLSModesPrivateCAClientCertificateAndHostnameVerification"
	for source, text := range map[string]string{
		"release script": releaseScript, "integration README": integrationReadme, "test commands": testCommands,
	} {
		if !strings.Contains(text, postgresTLS) {
			t.Fatalf("%s does not include PostgreSQL TLS contract %q", source, postgresTLS)
		}
	}
	for source, text := range map[string]string{"test commands": testCommands, "release gate": releaseDocs} {
		for _, coverage := range []string{"private", "hostname", "client-certificate"} {
			if !strings.Contains(text, coverage) {
				t.Fatalf("%s does not document PostgreSQL TLS %s coverage", source, coverage)
			}
		}
	}
	if strings.Contains(testCommands, "exact filter `^TestContainerDBDriver`") || strings.Contains(releaseDocs, "all-DB smoke path") {
		t.Fatal("DB gate documentation retained the superseded selection")
	}
}

func TestPublicOSSBoundaryDocsDoNotDefineManagedOperatingPolicy(t *testing.T) {
	root := repoRoot(t)
	paths := []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "docs", "app", "operations", "deployment", "page.mdx"),
		filepath.Join(root, "docs", "app", "architecture", "page.mdx"),
		filepath.Join(root, "docs", "app", "security", "page.mdx"),
	}
	for _, path := range paths {
		content := strings.ToLower(readText(t, path))
		for _, forbidden := range []string{
			"monitor agent connectivity",
			"handle patching",
			"retain operational logs",
			"backend admin ui",
			"managed dashboard is outside the oss core and is read-only",
			"dashboard does not issue api keys",
		} {
			if strings.Contains(content, forbidden) {
				t.Fatalf("%s defines managed-only policy %q", path, forbidden)
			}
		}
	}
}

func readWorkflow(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workflow map[string]any
	if err := yaml.Unmarshal(b, &workflow); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return workflow
}

func workflowSection(t *testing.T, workflow map[string]any, name string) map[string]any {
	t.Helper()
	section, ok := workflow[name].(map[string]any)
	if !ok {
		t.Fatalf("workflow section %q missing or invalid: %#v", name, workflow[name])
	}
	return section
}

func assertMainWorkflowTriggers(t *testing.T, workflow map[string]any) {
	t.Helper()
	events := workflowSection(t, workflow, "on")
	for _, event := range []string{"pull_request", "push"} {
		config, ok := events[event].(map[string]any)
		if !ok {
			t.Fatalf("workflow %s trigger missing configuration", event)
		}
		branches, ok := config["branches"].([]any)
		if !ok || len(branches) != 1 || branches[0] != "main" {
			t.Fatalf("workflow %s branches = %#v, want [main]", event, config["branches"])
		}
	}
	if _, ok := events["workflow_dispatch"]; !ok {
		t.Fatal("workflow missing workflow_dispatch trigger")
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
