package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentProcessRoutingAndStartupFailureDoNotLeakInput(t *testing.T) {
	dir := t.TempDir()
	name := "onprest-agent"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(dir, name)
	build := exec.Command("go", "build", "-trimpath", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}

	for _, args := range [][]string{{"unknown"}, {"--unknown"}} {
		cmd := exec.Command(binary, args...)
		output, err := cmd.CombinedOutput()
		if exitCode(err) != 2 || !strings.Contains(string(output), "invalid command or arguments") {
			t.Fatalf("args=%v exit=%d output=%q", args, exitCode(err), output)
		}
	}

	sentinel := "PRIVATE_CONFIG_SENTINEL"
	config := filepath.Join(dir, "invalid.yaml")
	content := "gateway:\n  url: ws://127.0.0.1:1/ws/agent\n  agent_private_key: key\ndatabase:\n  driver: postgres\n  host: localhost\n  port: " + sentinel + "\n"
	if err := os.WriteFile(config, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--config", config}, {"validate", "--config", config}, {"validate", "--config", config, "--format", "json"}} {
		cmd := exec.Command(binary, args...)
		output, err := cmd.CombinedOutput()
		if exitCode(err) != 1 {
			t.Fatalf("args=%v exit=%d output=%q", args, exitCode(err), output)
		}
		if strings.Contains(string(output), sentinel) {
			t.Fatalf("args=%v leaked config input: %q", args, output)
		}
	}
}

func TestUnsafeConfigurationKeysStaySingleRecordInRealProcess(t *testing.T) {
	dir := t.TempDir()
	name := "onprest-agent"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(dir, name)
	build := exec.Command("go", "build", "-trimpath", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	const sentinel = "RAW_KEY_SENTINEL"
	config := filepath.Join(dir, "unsafe-key.yaml")
	content := `gateway:
  url: ws://127.0.0.1:1/ws/agent
  agent_private_key: keEk2aSPeUHiCbhK-XxleMUFj3cwzcJCFUflKSs_CiZOsybztXdoRPcyYZTMd_f9cplE8Qd7VsMz484fWauOvw
database: {driver: postgres, host: localhost, port: 5432, name: test, user: test}
capabilities:
  safe:
    sql: select 1
    params:
      "bad\n` + sentinel + `\x1b[31m": {type: unsupported}
`
	if err := os.WriteFile(config, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		json bool
	}{
		{"startup", []string{"--config", config}, false},
		{"text", []string{"validate", "--config", config}, false},
		{"json", []string{"validate", "--config", config, "--format", "json"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(binary, tc.args...)
			output, err := cmd.CombinedOutput()
			if exitCode(err) != 1 || strings.Contains(string(output), sentinel) || strings.ContainsRune(string(output), '\x1b') {
				t.Fatalf("exit=%d output=%q", exitCode(err), output)
			}
			lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
			if len(lines) != 1 {
				t.Fatalf("output contains %d physical records: %q", len(lines), output)
			}
			if tc.json {
				var object map[string]any
				if err := json.Unmarshal(output, &object); err != nil {
					t.Fatalf("invalid JSON failure object: %v %q", err, output)
				}
				message, _ := object["message"].(string)
				if object["valid"] != false || !strings.Contains(message, "<invalid-key>") {
					t.Fatalf("invalid JSON failure object: %q", output)
				}
			} else if !strings.Contains(string(output), "<invalid-key>") {
				t.Fatalf("text failure did not use fixed key placeholder: %q", output)
			}
		})
	}
}

func TestNormalStartupDatabaseOpenFailureKeepsPrivateDetailLocal(t *testing.T) {
	dir := t.TempDir()
	name := "onprest-agent"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(dir, name)
	build := exec.Command("go", "build", "-trimpath", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	const sentinel = "PRIVATE_OPEN_SENTINEL"
	config := filepath.Join(dir, "open-failure.yaml")
	content := `gateway:
  url: ws://127.0.0.1:1/ws/agent
  agent_private_key: keEk2aSPeUHiCbhK-XxleMUFj3cwzcJCFUflKSs_CiZOsybztXdoRPcyYZTMd_f9cplE8Qd7VsMz484fWauOvw
database:
  driver: postgres
  host: localhost
  port: 5432
  name: test
  user: test
  tls:
    mode: verify-full
    ca_file: /definitely-missing/` + sentinel + `.pem
capabilities:
  safe: {sql: "select 1"}
`
	if err := os.WriteFile(config, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config", config)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 || strings.Contains(string(output), sentinel) || !strings.Contains(string(output), "agent startup failed") || strings.Count(string(output), "\n") != 1 {
		t.Fatalf("exit=%d output=%q", exitCode(err), output)
	}
	detail, err := os.ReadFile(binary + ".log")
	if err != nil || !strings.Contains(string(detail), sentinel) {
		t.Fatalf("local detail=%q err=%v", detail, err)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}
