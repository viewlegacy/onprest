//go:build integration

package it

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
	"github.com/viewlegacy/onprest/internal/gateway"
)

func TestProvisioningCLIOutputsRunGatewayAndAgentBinaries(t *testing.T) {
	db := postgresContainerConfig(t)
	repo := repoRoot(t)
	tmp := t.TempDir()
	gatewayBin := filepath.Join(tmp, "onprest-gateway")
	agentBin := filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	var agentOut, keyOut, stderr bytes.Buffer
	gateway.HandleCLI([]string{"create-agent-secret"}, &agentOut, &stderr)
	gateway.HandleCLI([]string{"create-key", "--name", "provisioned", "--capabilities", "echo_customer"}, &keyOut, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("provisioning CLI stderr: %s", stderr.String())
	}
	var agentKeys struct {
		PublicKey  string `json:"agent_public_key"`
		PrivateKey string `json:"agent_private_key"`
	}
	if err := json.Unmarshal(agentOut.Bytes(), &agentKeys); err != nil {
		t.Fatal(err)
	}
	var apiKey struct {
		Name         string          `json:"name"`
		APIKey       string          `json:"api_key"`
		KeyHash      string          `json:"key_hash"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(keyOut.Bytes(), &apiKey); err != nil {
		t.Fatal(err)
	}

	addr := freeAddr(t)
	probe, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	var gatewayConnections atomic.Int64
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		for {
			conn, err := probe.Accept()
			if err != nil {
				return
			}
			gatewayConnections.Add(1)
			_ = conn.Close()
		}
	}()
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", agentKeys.PrivateKey, `  echo_customer:
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
        type: integer`)
	validate := exec.Command(agentBin, "validate", "--config", capabilityFile, "--format", "json")
	validate.Dir = t.TempDir()
	validateOutput, err := validate.CombinedOutput()
	if err != nil {
		t.Fatalf("validate before Gateway startup: %v\n%s", err, validateOutput)
	}
	if got := string(validateOutput); !strings.Contains(got, `"valid":true`) || !strings.Contains(got, `"database_driver":"postgres"`) || strings.Contains(got, "agent_ready") || strings.Contains(got, "gateway_") {
		t.Fatalf("validate output=%s", got)
	}
	if _, err := os.Stat(filepath.Join(tmp, "onprest-agent.validate.log")); !os.IsNotExist(err) {
		t.Fatalf("validate success left detail log: %v", err)
	}

	pingSentinel := "private_ping_sentinel"
	missingDB := db
	missingDB.Name = pingSentinel
	pingConfig := writePostgresCapability(t, t.TempDir(), missingDB, "ws://"+addr+"/ws/agent", agentKeys.PrivateKey, capabilityBlock("ping_failure", "select 1::int as id"))
	startupPing := exec.Command(agentBin, "--config", pingConfig)
	startupPingOutput, startupPingErr := startupPing.CombinedOutput()
	if exitCodeOf(startupPingErr) != 1 || bytes.Contains(startupPingOutput, []byte(pingSentinel)) || !bytes.Contains(startupPingOutput, []byte("agent startup failed")) {
		t.Fatalf("normal startup ping failure exit=%d output=%s", exitCodeOf(startupPingErr), startupPingOutput)
	}
	explainSentinel := "private_explain_sentinel"
	explainConfig := writePostgresCapability(t, t.TempDir(), db, "ws://"+addr+"/ws/agent", agentKeys.PrivateKey, `  explain_failure:
    sql: select * from `+explainSentinel+`
    policy: {readonly: true, timeout: 2s, max_rows: 1, max_bytes: 128KB}
    result: {id: {type: integer}}`)
	startupExplain := exec.Command(agentBin, "--config", explainConfig)
	startupExplainOutput, startupExplainErr := startupExplain.CombinedOutput()
	if exitCodeOf(startupExplainErr) != 1 || bytes.Contains(startupExplainOutput, []byte(explainSentinel)) || !bytes.Contains(startupExplainOutput, []byte("agent startup failed")) {
		t.Fatalf("normal startup EXPLAIN failure exit=%d output=%s", exitCodeOf(startupExplainErr), startupExplainOutput)
	}
	_ = probe.Close()
	<-probeDone
	if got := gatewayConnections.Load(); got != 0 {
		t.Fatalf("validate attempted %d Gateway connection(s)", got)
	}
	gatewayCmd, _ := startProcessWithOutput(t, tmp, gatewayBin, nil, []string{
		"GATEWAY_ADDR=" + addr,
		"GATEWAY_AGENT_PUBLIC_KEY=" + agentKeys.PublicKey,
		"GATEWAY_API_KEYS_JSON=[{\"name\":" + quoteJSON(apiKey.Name) + ",\"key_hash\":" + quoteJSON(apiKey.KeyHash) + ",\"capabilities\":" + string(apiKey.Capabilities) + "}]",
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
		"GATEWAY_RATE_LIMIT_BURST=100",
	})
	defer stopProcess(t, gatewayCmd)
	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)
	agentCmd, _ := startProcessWithOutput(t, tmp, agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + capabilityFile})
	defer stopProcess(t, agentCmd)
	waitForHTTP(t, baseURL+"/openapi.json", apiKey.APIKey, http.StatusOK)

	status, body := postCapability(t, baseURL, apiKey.APIKey, "echo_customer", `{"id":42}`)
	if status != http.StatusOK {
		t.Fatalf("echo_customer status=%d body=%s", status, string(body))
	}
	if !strings.Contains(string(body), `"count":1`) || !strings.Contains(string(body), `"id":42`) {
		t.Fatalf("unexpected provisioned REST body: %s", string(body))
	}
}

func TestAgentCapabilityRestartUpdatesGatewayMetadata(t *testing.T) {
	db := postgresContainerConfig(t)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	tmp := t.TempDir()
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, capabilityBlock("first_capability", "select 1::int as id"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, time.Second)
	runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stopFirst := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(runCtx) }()
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)
	if got := string(getWithAPIKey(t, baseURL+"/openapi.json", secrets.APIKey)); !strings.Contains(got, "first_capability") {
		t.Fatalf("first OpenAPI = %s", got)
	} else if strings.Contains(got, "select 1") || strings.Contains(got, db.Host) || (db.Password != "" && strings.Contains(got, db.Password)) {
		t.Fatalf("OpenAPI leaked SQL or DB connection detail: %s", got)
	}
	stopFirst()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("first agent did not stop")
	}
	waitForAgentDisconnected(t, baseURL)
	if status, body := getWithAPIKeyStatus(t, baseURL+"/openapi.json", secrets.APIKey); status != http.StatusServiceUnavailable || strings.Contains(string(body), "first_capability") {
		t.Fatalf("disconnected OpenAPI status=%d body=%s", status, string(body))
	}

	writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, capabilityBlock("second_capability", "select 2::int as id"))
	runner, err = agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	errCh = make(chan error, 1)
	go func() { errCh <- runner.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, body := getWithAPIKeyStatus(t, baseURL+"/openapi.json", secrets.APIKey)
		got := string(body)
		if status != http.StatusOK {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if strings.Contains(got, "second_capability") && !strings.Contains(got, "first_capability") {
			tools := postMCPPayload(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			if !strings.Contains(string(tools), "second_capability") || strings.Contains(string(tools), "first_capability") {
				t.Fatalf("tools/list did not update after agent restart: %s", string(tools))
			}
			status, body := postCapability(t, baseURL, secrets.APIKey, "second_capability", `{}`)
			if status != http.StatusOK || !strings.Contains(string(body), `"id":2`) {
				t.Fatalf("second_capability REST status=%d body=%s", status, string(body))
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("gateway metadata did not update after agent restart")
}

func TestAgentInvalidCapabilityRestartDoesNotConnect(t *testing.T) {
	db := postgresContainerConfig(t)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	tmp := t.TempDir()
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, capabilityBlock("valid_capability", "select 1::int as id"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, time.Second)
	runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(runCtx) }()
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)
	stop()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("valid agent did not stop")
	}
	waitForAgentDisconnected(t, baseURL)

	writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, `  invalid_write:
    sql: update customers set name = 'bad'
    policy:
      readonly: true
      timeout: 1s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer`)
	if _, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil); err == nil {
		t.Fatal("NewRunner succeeded with invalid readonly update")
	}
	waitForHealthAgentState(t, baseURL, false)
}

func TestAgentProcessSignalAndLocalDetailLog(t *testing.T) {
	db := postgresContainerConfig(t)
	repo := repoRoot(t)
	tmp := t.TempDir()
	agentBin := filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	secrets := newITSecrets(t)
	addr := freeAddr(t)
	capabilityFile := writePostgresCapabilityWithLogging(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, "512B", 2, `  failing_query:
    sql: select (1 / :denominator::int)::int as value
    params:
      denominator:
        type: integer
        required: true
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      value:
        type: integer`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, time.Second)
	cmd, output := startProcessWithOutput(t, tmp, agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + capabilityFile})
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)
	var body []byte
	for i := 0; i < 8; i++ {
		status, got := postCapability(t, baseURL, secrets.APIKey, "failing_query", `{"denominator":0}`)
		body = got
		if status != http.StatusBadGateway {
			t.Fatalf("failing_query status=%d body=%s", status, string(body))
		}
		requireAPIErrorCode(t, body, "AGENT_QUERY_FAILED")
	}

	logPath := agentBin + ".log"
	logs, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read agent local log: %v; stdout=%s", err, output.String())
	}
	if !bytes.Contains(logs, []byte("AGENT_QUERY_FAILED")) || !bytes.Contains(logs, []byte("division")) {
		t.Fatalf("agent local log missing detail: %s", string(logs))
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("agent local log did not rotate to %s.1: %v; current=%s", logPath, err, string(logs))
	}
	if strings.Contains(output.String(), string(db.Password)) {
		t.Fatalf("agent stdout leaked DB password: %s", output.String())
	}
	events := parseJSONLines(t, output.String())
	foundReady := false
	foundConnected := false
	for _, event := range events {
		if event["event"] == "agent_ready" {
			foundReady = true
		}
		if event["event"] == "gateway_connected" {
			foundConnected = true
		}
	}
	if !foundReady || !foundConnected {
		t.Fatalf("agent stdout missing lifecycle JSON events: %s", output.String())
	}

	_ = cmd.Process.Signal(os.Interrupt)
	if err := waitForExit(t, cmd, 5*time.Second); err != nil {
		t.Fatalf("agent did not exit cleanly: %v\n%s", err, output.String())
	}
}

func TestAgentProcessReconnectsWithDefaultIntervalAfterGatewayStarts(t *testing.T) {
	db := postgresContainerConfig(t)
	repo := repoRoot(t)
	tmp := t.TempDir()
	agentBin := filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	secrets := newITSecrets(t)
	addr := freeAddr(t)
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, capabilityBlock("reconnect_capability", "select 1::int as id"))
	agentCmd, _ := startProcessWithOutput(t, tmp, agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + capabilityFile})
	defer stopProcess(t, agentCmd)

	time.Sleep(500 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, time.Second)
	waitForHTTPWithin(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK, 35*time.Second)
}

func waitForHTTPWithin(t *testing.T, url, apiKey string, wantStatus int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == wantStatus {
				return
			}
			last = string(body)
		} else {
			last = err.Error()
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %s", url, last)
}

func TestAgentRejectsUnknownCapabilityFromGateway(t *testing.T) {
	db := postgresContainerConfig(t)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	tmp := t.TempDir()
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, capabilityBlock("known_capability", "select 1::int as id"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, time.Second)
	runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(ctx) }()
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

	status, body := postCapability(t, baseURL, secrets.APIKey, "unknown_capability", `{}`)
	if status != http.StatusNotFound {
		t.Fatalf("unknown status=%d body=%s", status, string(body))
	}
	requireAPIErrorCode(t, body, "GATEWAY_CAPABILITY_NOT_FOUND")
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}

func TestPostgresReadOnlyDBUserAllowsReadAndBlocksMutationAtStartup(t *testing.T) {
	db := postgresReadOnlyContainerConfig(t)
	repo := repoRoot(t)
	secrets := newITSecrets(t)
	tmp := t.TempDir()
	agentBin := filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	addr := freeAddr(t)
	readFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, capabilityBlock("read_current_user", "select 1::int as id"))
	runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: readFile, ReconnectEvery: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("readonly DB user could not initialize read capability: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	baseURL := startInternalGateway(t, ctx, addr, secrets, time.Second)
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(ctx) }()
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)
	status, body := postCapability(t, baseURL, secrets.APIKey, "read_current_user", `{}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"id":1`) {
		t.Fatalf("read_current_user status=%d body=%s", status, string(body))
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("readonly agent runner did not stop")
	}

	mutationFile := writePostgresCapability(t, tmp, db, "ws://127.0.0.1:1/ws/agent", secrets.AgentPrivateKey, `  forbidden_update:
    sql: update pg_class set relname = relname where false
    policy:
      readonly: false
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB`)
	cmd, output := startProcessWithOutput(t, tmp, agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + mutationFile})
	if err := waitForExit(t, cmd, 5*time.Second); err == nil {
		t.Fatal("readonly DB user initialized mutating capability, want DB permission failure")
	}
	startupLog, err := os.ReadFile(agentBin + ".log")
	if err != nil {
		t.Fatalf("read startup detail log: %v; output=%s", err, output.String())
	}
	if !bytes.Contains(startupLog, []byte("AGENT_STARTUP_FAILED")) ||
		!bytes.Contains(bytes.ToLower(startupLog), []byte("permission")) {
		t.Fatalf("startup detail log missing DB permission detail: %s", string(startupLog))
	}
	for _, leaked := range []string{db.Password, "permission denied", "pg_class"} {
		if leaked != "" && strings.Contains(strings.ToLower(output.String()), strings.ToLower(leaked)) {
			t.Fatalf("agent startup output leaked DB detail %q: %s", leaked, output.String())
		}
	}
}

func capabilityBlock(name, sql string) string {
	return "  " + name + ":\n" +
		"    sql: " + sql + "\n" +
		"    policy:\n" +
		"      readonly: true\n" +
		"      timeout: 2s\n" +
		"      max_rows: 1\n" +
		"      max_bytes: 128KB\n" +
		"    result:\n" +
		"      id:\n" +
		"        type: integer"
}
