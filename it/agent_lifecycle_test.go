//go:build integration

package it

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
)

func TestAgentExitsOnLintFailureWithoutConnecting(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	agentBin := filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	secrets := newITSecrets(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, 100*time.Millisecond)

	capabilityFile := writeFile(t, filepath.Join(tmp, "invalid-capability.yaml"), `database:
  driver: postgres
  host: localhost
  port: 5432
  name: legacy
  user: readonly_user
gateway:
  url: "ws://`+addr+`/ws/agent"
  agent_private_key: "`+secrets.AgentPrivateKey+`"
capabilities:
  invalid_write:
    sql: update customers set name = 'bad'
    policy:
      readonly: true
      timeout: 1s
      max_rows: 1
      max_bytes: 128KB
`)
	cmd := startProcess(t, context.Background(), agentBin, []string{"--config", capabilityFile}, nil)
	if err := waitForExit(t, cmd, 5*time.Second); err == nil {
		t.Fatal("agent exited successfully, want lint failure")
	}
	waitForHealthAgentState(t, baseURL, false)
}

func TestAgentExitsOnDBPingFailure(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	agentBin := filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	secrets := newITSecrets(t)
	closedAddr := freeAddr(t)
	host, port, ok := strings.Cut(closedAddr, ":")
	if !ok {
		t.Fatalf("unexpected addr: %s", closedAddr)
	}
	capabilityFile := writePostgresCapability(t, tmp, postgresConfig{
		Host: host,
		Port: port,
		Name: "missing",
		User: "missing",
	}, "ws://127.0.0.1:1/ws/agent", secrets.AgentPrivateKey, `  ping:
    sql: select 1::int as id
    policy:
      readonly: true
      timeout: 1s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer`)

	cmd := startProcess(t, context.Background(), agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + capabilityFile})
	if err := waitForExit(t, cmd, 5*time.Second); err == nil {
		t.Fatal("agent exited successfully, want DB ping failure")
	}
}

func TestAgentExitsOnExplainFailure(t *testing.T) {
	db := postgresContainerConfig(t)
	repo := repoRoot(t)
	tmp := t.TempDir()
	agentBin := filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	secrets := newITSecrets(t)
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://127.0.0.1:1/ws/agent", secrets.AgentPrivateKey, `  explain_fail:
    sql: select missing_column from missing_table
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      missing_column:
        type: string`)

	cmd := startProcess(t, context.Background(), agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + capabilityFile})
	if err := waitForExit(t, cmd, 5*time.Second); err == nil {
		t.Fatal("agent exited successfully, want EXPLAIN failure")
	}
}

func TestAgentReconnectsAfterGatewayStarts(t *testing.T) {
	db := postgresContainerConfig(t)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	tmp := t.TempDir()
	capabilityFile := renderCapability(t, repoRoot(t), tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey)

	runner, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(ctx) }()

	time.Sleep(250 * time.Millisecond)
	baseURL := startInternalGateway(t, ctx, addr, secrets, 500*time.Millisecond)
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}

func TestSameAgentRunnerReconnectsAfterGatewayProcessRestart(t *testing.T) {
	db := postgresContainerConfig(t)
	repo := repoRoot(t)
	tmp := t.TempDir()
	gatewayBin := filepath.Join(tmp, "onprest-gateway")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")

	secrets := newITSecrets(t)
	addr := freeAddr(t)
	baseURL := "http://" + addr
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, capabilityBlock("reconnect_capability", "select 1::int as id"))
	gatewayEnv := []string{
		"GATEWAY_ADDR=" + addr,
		"GATEWAY_AGENT_PUBLIC_KEY=" + secrets.AgentPublicKey,
		"GATEWAY_API_KEYS_JSON=" + secrets.APIKeysJSON,
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
		"GATEWAY_RATE_LIMIT_BURST=100",
	}

	gatewayCmd, _ := startProcessWithOutput(t, tmp, gatewayBin, nil, gatewayEnv)
	defer func() { stopProcess(t, gatewayCmd) }()
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)

	runner, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	runnerCtx, cancelRunner := context.WithCancel(context.Background())
	runnerErr := make(chan error, 1)
	go func() { runnerErr <- runner.Run(runnerCtx) }()
	defer func() {
		cancelRunner()
		select {
		case <-runnerErr:
		case <-time.After(5 * time.Second):
			t.Error("agent runner did not stop after gateway restart test")
		}
	}()

	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)
	status, body := postCapability(t, baseURL, secrets.APIKey, "reconnect_capability", `{}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"id":1`) {
		t.Fatalf("pre-restart capability status=%d body=%s", status, body)
	}

	stopProcess(t, gatewayCmd)
	gatewayCmd = nil
	select {
	case err := <-runnerErr:
		t.Fatalf("agent runner exited after gateway stopped instead of reconnecting: %v", err)
	default:
	}

	gatewayCmd, _ = startProcessWithOutput(t, tmp, gatewayBin, nil, gatewayEnv)
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)
	status, body = postCapability(t, baseURL, secrets.APIKey, "reconnect_capability", `{}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"id":1`) {
		t.Fatalf("post-restart capability status=%d body=%s", status, body)
	}
}

func TestAgentDefaultReconnectIntervalIsThirtySeconds(t *testing.T) {
	cfg, err := agentpkg.LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReconnectEvery != 30*time.Second {
		t.Fatalf("ReconnectEvery = %s, want 30s", cfg.ReconnectEvery)
	}
}

func TestCapabilityFileChangesRequireRestart(t *testing.T) {
	db := postgresContainerConfig(t)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	tmp := t.TempDir()
	capabilityFile := renderCapability(t, repoRoot(t), tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, 500*time.Millisecond)
	runner, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(ctx) }()
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

	f, err := os.OpenFile(capabilityFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`
  added_after_start:
    sql: select 1::int as id
    policy:
      readonly: true
      timeout: 1s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer
`)
	_ = f.Close()

	time.Sleep(300 * time.Millisecond)
	status, body := postCapability(t, baseURL, secrets.APIKey, "added_after_start", `{}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", status, http.StatusNotFound, string(body))
	}
	requireAPIErrorCode(t, body, "GATEWAY_CAPABILITY_NOT_FOUND")

	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}
