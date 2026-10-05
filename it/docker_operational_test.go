//go:build integration

package it

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
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
	}, "ws://gateway:8080/ws/agent", secrets.AgentPrivateKey, `get_customer:
  policy:
    readonly: true
    timeout: 2s
    max_rows: 1
    max_output_bytes: 128KB
  executor:
    type: db
    source: primary
    sql: select :id::int as id
  input:
    properties:
      id:
        type: integer
    required:
      - id
  output:
    data: {}
    items:
      properties:
        id:
          type: integer
`)
	if err := os.Chmod(capabilityFile, 0o644); err != nil {
		t.Fatalf("chmod compose capability file: %v", err)
	}
	file, err := os.OpenFile(capabilityFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("runtime: {identity_dir: /var/lib/onprest/identity, spool_dir: /var/lib/onprest/spool}\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("write protected runtime settings")
	}
	identityVolume := fmt.Sprintf("onprest-it-identity-%x", time.Now().UnixNano())
	if err := exec.Command("docker", "volume", "create", "--label", "onprest.it=p04-identity", identityVolume).Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", identityVolume).Run() })
	spoolVolume := fmt.Sprintf("onprest-it-spool-%x", time.Now().UnixNano())
	if err := exec.Command("docker", "volume", "create", "--label", "onprest.it=p07-spool", spoolVolume).Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", spoolVolume).Run() })

	// Operator preparation uses the already-required DB image. The Agent itself
	// continues to run as the image's nonroot user with a private writable mount.
	if out, err := exec.Command("docker", "run", "--rm", "--network", "none", "--user", "0", "--entrypoint", "sh", "-v", identityVolume+":/identity", "-v", spoolVolume+":/spool", "postgres:16-alpine", "-c", "chown 65532:65532 /identity /spool && chmod 700 /identity /spool").CombinedOutput(); err != nil {
		t.Fatalf("prepare protected runtime volumes: %v %s", err, out)
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
      - identity:/var/lib/onprest/identity
      - spool:/var/lib/onprest/spool
volumes:
  identity:
    external: true
    name: `+identityVolume+`
  spool:
    external: true
    name: `+spoolVolume+`
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
	t.Cleanup(func() {
		down := exec.Command("docker", "compose", "-f", composeFile, "down", "-v", "--remove-orphans")
		down.Dir = tmp
		_ = down.Run()
	})
	if err := up.Run(); err != nil {
		t.Fatalf("docker compose up failed: %v\n%s", err, output.String())
	}
	baseURL := "http://" + composeServicePort(t, tmp, composeFile, "gateway", "8080")
	waitForComposeOpenAPI(t, tmp, composeFile, baseURL, secrets.APIKey, 45*time.Second)
	checkCalls := func() {
		t.Helper()
		status, body := postCapability(t, baseURL, secrets.APIKey, "get_customer", `{"id":7}`)
		if status != http.StatusOK || !strings.Contains(string(body), `"id":7`) {
			t.Fatalf("compose REST status=%d body=%s", status, string(body))
		}
		mcpBody := postMCPPayload(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{"id":8}}}`)
		response := requireMCPToolCallResponse(t, mcpBody, "1", false)
		var textResult mcpRowsResult
		decodeMCPJSON(t, []byte(response.Result.Content[0].Text), &textResult)
		if textResult.Count.String() != "1" || len(textResult.Rows) != 1 || textResult.Rows[0].ID.String() != "8" {
			t.Fatalf("compose MCP tools/call text result=%#v", textResult)
		}
	}
	checkCalls()
	assertComposeSpoolProtected(t, spoolVolume)
	beforeHash := composeIdentityDefinitionHash(t, identityVolume, capabilityFile)
	oldID := composeAgentID(t, tmp, composeFile)
	recreate := exec.Command("docker", "compose", "-f", composeFile, "up", "-d", "--no-deps", "--force-recreate", "agent")
	recreate.Dir = tmp
	if out, err := recreate.CombinedOutput(); err != nil {
		t.Fatalf("recreate agent: %v %s", err, out)
	}
	if composeAgentID(t, tmp, composeFile) == oldID {
		t.Fatal("Agent container was not recreated")
	}
	waitForComposeOpenAPI(t, tmp, composeFile, baseURL, secrets.APIKey, 45*time.Second)
	checkCalls()
	assertComposeSpoolProtected(t, spoolVolume)
	if composeIdentityDefinitionHash(t, identityVolume, capabilityFile) != beforeHash {
		t.Fatal("persistent identity lost the prepared definition binding")
	}
}

func assertComposeSpoolProtected(t *testing.T, volume string) {
	t.Helper()
	cmd := exec.Command("docker", "run", "--rm", "--network", "none", "--user", "65532:65532", "--entrypoint", "sh", "-v", volume+":/spool:ro", "postgres:16-alpine", "-c", "stat -c '%u:%g:%a' /spool /spool/objects /spool/.lock")
	out, e := cmd.Output()
	if e != nil || string(out) != "65532:65532:700\n65532:65532:700\n65532:65532:600\n" {
		t.Fatal("runtime spool ownership or protection is invalid", e)
	}
}

func composeAgentID(t *testing.T, dir, composeFile string) string {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", composeFile, "ps", "-q", "agent")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		t.Fatal("read Agent container identity")
	}
	return strings.TrimSpace(string(out))
}

// Inspect the protected persistent key without logging its bytes, then use the
// production snapshot resolver to compare the definition binding across recreate.
func composeIdentityDefinitionHash(t *testing.T, volume, configFile string) string {
	t.Helper()
	cmd := exec.Command("docker", "run", "--rm", "--network", "none", "--user", "65532:65532", "--entrypoint", "sh", "-v", volume+":/identity:ro", "postgres:16-alpine", "-c", "stat -c '%u:%g:%a' /identity /identity/config-hmac.key && cat /identity/config-hmac.key")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal("inspect protected persistent identity")
	}
	defer clear(out)
	parts := bytes.SplitN(out, []byte{'\n'}, 3)
	if len(parts) != 3 || string(parts[0]) != "65532:65532:700" || string(parts[1]) != "65532:65532:600" || len(parts[2]) != 32 {
		t.Fatal("identity directory/key ownership, permissions or size is invalid")
	}
	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := contract.Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	identity := t.TempDir()
	if err := os.Chmod(identity, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(identity, "config-hmac.key"), parts[2], 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Runtime["identity_dir"] = identity
	snapshot, err := providers.Resolve(context.Background(), cfg, configFile, "get_customer", providers.ResolveOptions{Environment: providers.ProcessEnvironment()})
	if err != nil {
		t.Fatal("resolve persistent definition binding")
	}
	return snapshot.DefinitionHash()
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
