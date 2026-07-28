//go:build integration

package it

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerTargetsBuildWhenDockerIntegrationEnabled(t *testing.T) {
	if os.Getenv("ONPREST_IT_DOCKER") != "1" {
		t.Skip("set ONPREST_IT_DOCKER=1 to run Docker operational tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	repo := repoRoot(t)
	for _, target := range []string{"gateway", "agent"} {
		t.Run(target, func(t *testing.T) {
			tag := "onprest-it-" + target
			cmd := exec.Command("docker", "build", "--build-arg", "TARGET="+target, "-t", tag, repo)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Run(); err != nil {
				t.Fatalf("docker build target %s failed: %v\n%s", target, err, output.String())
			}
			inspect := exec.Command("docker", "image", "inspect", tag)
			output.Reset()
			inspect.Stdout = &output
			inspect.Stderr = &output
			if err := inspect.Run(); err != nil {
				t.Fatalf("docker image inspect %s failed: %v\n%s", tag, err, output.String())
			}
			if target == "gateway" {
				secrets := newITSecrets(t)
				run := exec.Command("docker", "run", "--rm", "-d", "-p", "127.0.0.1::8080",
					"-e", "GATEWAY_AGENT_PUBLIC_KEY="+secrets.AgentPublicKey,
					"-e", "GATEWAY_API_KEYS_JSON="+secrets.APIKeysJSON,
					"-e", "GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
					"-e", "GATEWAY_RATE_LIMIT_BURST=100",
					tag)
				output.Reset()
				run.Stdout = &output
				run.Stderr = &output
				if err := run.Run(); err != nil {
					t.Fatalf("docker run gateway failed: %v\n%s", err, output.String())
				}
				containerID := strings.TrimSpace(output.String())
				t.Cleanup(func() { _ = exec.Command("docker", "stop", containerID).Run() })
				portOut, err := exec.Command("docker", "port", containerID, "8080/tcp").Output()
				if err != nil {
					t.Fatalf("docker port gateway failed: %v", err)
				}
				addr := strings.TrimSpace(string(portOut))
				waitForHTTPWithin(t, "http://"+addr+"/healthz", "", 200, 20*time.Second)
			} else {
				run := exec.Command("docker", "run", "--rm", tag)
				output.Reset()
				run.Stdout = &output
				run.Stderr = &output
				if err := run.Run(); err == nil {
					t.Fatalf("docker run agent without capability file succeeded, want config failure proving entrypoint executed")
				}
				if !strings.Contains(output.String(), "agent init") {
					t.Fatalf("agent container did not appear to execute agent entrypoint: %s", output.String())
				}
			}
		})
	}
}

func TestDockerComposeEnvFilePreservesGatewayAPIKeysJSON(t *testing.T) {
	if os.Getenv("ONPREST_IT_DOCKER_COMPOSE") != "1" {
		t.Skip("set ONPREST_IT_DOCKER_COMPOSE=1 to run Docker Compose operational tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	repo := repoRoot(t)
	tmp := t.TempDir()
	secrets := newITSecrets(t)
	for _, target := range []string{"gateway", "agent"} {
		tag := "onprest-it-" + target
		cmd := exec.Command("docker", "build", "--build-arg", "TARGET="+target, "-t", tag, repo)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Run(); err != nil {
			t.Fatalf("docker build target %s failed: %v\n%s", target, err, output.String())
		}
	}
	envFile := filepath.Join(tmp, "gateway.env")
	apiKeysJSON := "'[{\"name\":\"it\",\"key_hash\":\"" + secrets.APIKeyHash + "\",\"capabilities\":[\"*\"]}]'"
	writeFile(t, envFile, "GATEWAY_AGENT_PUBLIC_KEY="+secrets.AgentPublicKey+"\n"+
		"GATEWAY_API_KEYS_JSON="+apiKeysJSON+"\n"+
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100\n"+
		"GATEWAY_RATE_LIMIT_BURST=100\n")
	capabilityFile := writePostgresCapability(t, tmp, postgresConfig{
		Host:     "db",
		Port:     "5432",
		Name:     itDBName,
		User:     itDBUser,
		Password: itDBPassword,
	}, "ws://gateway:8080/ws/agent", secrets.AgentPrivateKey, `  get_customer:
    sql: select :id::int as id
    params:
      id:
        type: integer
        required: true
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer
`)
	if err := os.Chmod(capabilityFile, 0o644); err != nil {
		t.Fatalf("chmod compose capability file: %v", err)
	}
	composeFile := filepath.Join(tmp, "compose.yaml")
	writeFile(t, composeFile, `services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: `+itDBName+`
      POSTGRES_USER: `+itDBUser+`
      POSTGRES_PASSWORD: `+itDBPassword+`
    healthcheck:
      test: ["CMD-SHELL", "psql -U `+itDBUser+` -d `+itDBName+` -c 'SELECT 1' >/dev/null"]
      interval: 1s
      timeout: 5s
      retries: 30
  gateway:
    image: onprest-it-gateway
    env_file:
      - gateway.env
    environment:
      GATEWAY_ADDR: ":8080"
    ports:
      - "127.0.0.1::8080"
  agent:
    image: onprest-it-agent
    depends_on:
      db:
        condition: service_healthy
      gateway:
        condition: service_started
    environment:
      AGENT_CAPABILITY_FILE: /config/capability.yaml
    volumes:
      - ./`+filepath.Base(capabilityFile)+`:/config/capability.yaml:ro
`)
	configFile := filepath.Join(tmp, "compose-config.yaml")
	writeFile(t, configFile, `services:
  gateway:
    image: onprest-it-gateway
    env_file:
      - gateway.env
`)
	cmd := exec.Command("docker", "compose", "--env-file", envFile, "-f", configFile, "config")
	cmd.Dir = tmp
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker compose config failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "$2") || !strings.Contains(output.String(), "GATEWAY_API_KEYS_JSON") {
		t.Fatalf("compose config did not preserve gateway API key JSON/hash: %s", output.String())
	}
	up := exec.Command("docker", "compose", "-f", composeFile, "up", "-d")
	up.Dir = tmp
	output.Reset()
	up.Stdout = &output
	up.Stderr = &output
	if err := up.Run(); err != nil {
		t.Fatalf("docker compose up failed: %v\n%s", err, output.String())
	}
	t.Cleanup(func() {
		down := exec.Command("docker", "compose", "-f", composeFile, "down", "-v", "--remove-orphans")
		down.Dir = tmp
		_ = down.Run()
	})
	baseURL := "http://" + composeServicePort(t, tmp, composeFile, "gateway", "8080")
	waitForComposeOpenAPI(t, tmp, composeFile, baseURL, secrets.APIKey, 45*time.Second)
	status, body := postCapability(t, baseURL, secrets.APIKey, "get_customer", `{"id":7}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"id":7`) {
		t.Fatalf("compose REST status=%d body=%s", status, string(body))
	}
	mcpBody := postMCPPayload(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{"id":8}}}`)
	if !strings.Contains(string(mcpBody), `"id":8`) {
		t.Fatalf("compose MCP tools/call body=%s", string(mcpBody))
	}
}

func composeServicePort(t *testing.T, dir, composeFile, service, port string) string {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", composeFile, "port", service, port)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose port %s %s failed: %v", service, port, err)
	}
	addr := strings.TrimSpace(string(out))
	addr = strings.TrimPrefix(addr, "0.0.0.0:")
	if !strings.Contains(addr, ":") {
		return "127.0.0.1:" + addr
	}
	if strings.HasPrefix(addr, "[::]:") {
		return "127.0.0.1:" + strings.TrimPrefix(addr, "[::]:")
	}
	return addr
}

func waitForComposeOpenAPI(t *testing.T, dir, composeFile, baseURL, apiKey string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		status, body := getStatusWithAPIKey(t, baseURL+"/openapi.json", apiKey)
		if status == http.StatusOK {
			return
		}
		last = fmt.Sprintf("status=%d body=%s", status, string(body))
		time.Sleep(500 * time.Millisecond)
	}
	logs := exec.Command("docker", "compose", "-f", composeFile, "logs", "--no-color")
	logs.Dir = dir
	out, _ := logs.CombinedOutput()
	t.Fatalf("timed out waiting for compose OpenAPI: %s\ncompose logs:\n%s", last, string(out))
}
