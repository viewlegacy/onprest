//go:build integration

package it

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
	"golang.org/x/crypto/bcrypt"
)

func TestActualAgentValidationThroughRESTAndMCP(t *testing.T) {
	db := postgresContainerConfig(t)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	tmp := t.TempDir()
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, `  validated_lookup:
    sql: select :id::int as id
    params:
      id:
        type: integer
        required: true
        minimum: 1
        maximum: 10
      code:
        type: string
        required: true
        pattern: "^[A-Z]{3}$"
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &bytes.Buffer{}
	baseURL := startInternalGatewayWithLog(t, ctx, addr, secrets, time.Second, logs)
	runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(ctx) }()
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

	status, body := postCapability(t, baseURL, secrets.APIKey, "validated_lookup", `{"id":3,"code":"ABC"}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"id":3`) {
		t.Fatalf("validated_lookup status=%d body=%s", status, string(body))
	}
	for _, payload := range []string{
		`{"id":"leak-me","code":"ABC"}`,
		`{"id":99,"code":"ABC"}`,
		`{"id":1,"code":"ABC","secret":"leak-me"}`,
	} {
		status, body = postCapability(t, baseURL, secrets.APIKey, "validated_lookup", payload)
		if status != http.StatusBadRequest {
			t.Fatalf("invalid REST payload %s status=%d body=%s", payload, status, string(body))
		}
		requireAPIErrorCode(t, body, "AGENT_VALIDATION_FAILED")
		if strings.Contains(string(body), "leak-me") || strings.Contains(logs.String(), "leak-me") {
			t.Fatalf("validation detail leaked; body=%s logs=%s", string(body), logs.String())
		}
	}

	status, body = postMCPStatus(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"validated_lookup","arguments":{"id":1,"code":"leak-me"}}}`)
	if status != http.StatusOK {
		t.Fatalf("invalid MCP status=%d body=%s", status, string(body))
	}
	requireMCPToolErrorCode(t, body, "AGENT_VALIDATION_FAILED")
	if strings.Contains(string(body), "leak-me") || strings.Contains(logs.String(), "leak-me") {
		t.Fatalf("MCP validation detail leaked; body=%s logs=%s", string(body), logs.String())
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}

func TestAgentRejectsUnknownCapabilityFromCompromisedGateway(t *testing.T) {
	db := postgresContainerConfig(t)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	responseCh := make(chan protocol.Response, 1)
	srv := &http.Server{Addr: addr}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/ws/agent/challenge" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"challenge":"compromised-gateway-challenge"}`)
			return
		}
		conn, err := ws.Accept(w, r)
		if err != nil {
			t.Errorf("accept fake gateway websocket: %v", err)
			return
		}
		defer conn.Close()
		req := protocol.Request{ID: "unknown-1", Capability: "not_in_capability_yaml", Params: map[string]any{"secret": "gateway-only-secret"}}
		if err := conn.WriteText(protocol.MustJSON(req)); err != nil {
			t.Errorf("write unknown capability: %v", err)
			return
		}
		msg, err := conn.ReadText()
		if err != nil {
			t.Errorf("read unknown capability response: %v", err)
			return
		}
		var resp protocol.Response
		if err := json.Unmarshal(msg, &resp); err != nil {
			t.Errorf("decode unknown capability response: %v; msg=%s", err, string(msg))
			return
		}
		responseCh <- resp
	})
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	})

	tmp := t.TempDir()
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, capabilityBlock("known_capability", "select 1::int as id"))
	runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(ctx) }()

	var resp protocol.Response
	select {
	case resp = <-responseCh:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not respond to compromised gateway request")
	}
	if resp.Error == nil || resp.Error.Code != "GATEWAY_CAPABILITY_NOT_FOUND" {
		t.Fatalf("unknown capability response = %#v", resp)
	}
	if strings.Contains(resp.Error.Message, "gateway-only-secret") {
		t.Fatalf("unknown capability response leaked params: %#v", resp.Error)
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}

func TestDistCLIProvisioningRunsOutsideSourceTree(t *testing.T) {
	db := postgresContainerConfig(t)
	repo := repoRoot(t)
	tmp := t.TempDir()
	distDir := filepath.Join(tmp, "dist")
	runDir := filepath.Join(tmp, "run")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gatewayBin := filepath.Join(distDir, "onprest-gateway")
	agentBin := filepath.Join(distDir, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	var agentKeys struct {
		PublicKey  string `json:"agent_public_key"`
		PrivateKey string `json:"agent_private_key"`
	}
	runBinaryJSON(t, runDir, gatewayBin, []string{"create-agent"}, &agentKeys)
	var apiKey struct {
		Name         string          `json:"name"`
		APIKey       string          `json:"api_key"`
		KeyHash      string          `json:"key_hash"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	runBinaryJSON(t, runDir, gatewayBin, []string{"create-key", "--name", "provisioned", "--capabilities", "get_customer"}, &apiKey)

	addr := freeAddr(t)
	envFile := filepath.Join(runDir, "gateway.env")
	writeFile(t, envFile, "GATEWAY_ADDR="+addr+"\n"+
		"GATEWAY_AGENT_PUBLIC_KEY="+agentKeys.PublicKey+"\n"+
		"GATEWAY_API_KEYS_JSON='[{\"name\":"+quoteJSON(apiKey.Name)+",\"key_hash\":"+quoteJSON(apiKey.KeyHash)+",\"capabilities\":"+string(apiKey.Capabilities)+"}]'\n"+
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100\n"+
		"GATEWAY_RATE_LIMIT_BURST=100\n")
	capabilityFile := writePostgresCapability(t, runDir, db, "ws://"+addr+"/ws/agent", agentKeys.PrivateKey, `  get_customer:
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

	gatewayCmd, gatewayOut := startShellProcessWithOutput(t, runDir, "set -a; . "+shellQuote(envFile)+"; set +a; exec "+shellQuote(gatewayBin))
	defer stopProcess(t, gatewayCmd)
	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)
	agentCmd, agentOut := startProcessWithOutput(t, runDir, agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + capabilityFile})
	defer stopProcess(t, agentCmd)
	waitForHTTP(t, baseURL+"/openapi.json", apiKey.APIKey, http.StatusOK)

	status, body := postCapability(t, baseURL, apiKey.APIKey, "get_customer", `{"id":42}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"id":42`) {
		t.Fatalf("provisioned REST status=%d body=%s gateway=%s agent=%s", status, string(body), gatewayOut.String(), agentOut.String())
	}
	tools := postMCPPayload(t, baseURL, apiKey.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(string(tools), "get_customer") {
		t.Fatalf("provisioned tools/list = %s", string(tools))
	}
	mcpBody := postMCPPayload(t, baseURL, apiKey.APIKey, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_customer","arguments":{"id":43}}}`)
	if !strings.Contains(string(mcpBody), `"structuredContent"`) || !strings.Contains(string(mcpBody), `"id":43`) {
		t.Fatalf("provisioned tools/call = %s", string(mcpBody))
	}
}

func TestDedicatedGatewayProcessesDoNotShareState(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	gatewayBin := filepath.Join(tmp, "onprest-gateway")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")

	a := newITSecrets(t)
	b := newITSecrets(t)
	b.APIKey = "tenant-b-api-key"
	hash, err := bcrypt.GenerateFromPassword([]byte(b.APIKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	b.APIKeyHash = string(hash)
	b.APIKeysJSON = `[{"name":"it","key_hash":` + quoteJSON(b.APIKeyHash) + `,"capabilities":["*"]}]`

	addrA := freeAddr(t)
	addrB := freeAddr(t)
	runA := filepath.Join(tmp, "tenant-a")
	runB := filepath.Join(tmp, "tenant-b")
	if err := os.MkdirAll(runA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runB, 0o755); err != nil {
		t.Fatal(err)
	}
	cmdA, outA := startProcessWithOutput(t, runA, gatewayBin, nil, []string{
		"GATEWAY_ADDR=" + addrA,
		"GATEWAY_AGENT_PUBLIC_KEY=" + a.AgentPublicKey,
		"GATEWAY_API_KEYS_JSON=" + a.APIKeysJSON,
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
		"GATEWAY_RATE_LIMIT_BURST=100",
	})
	defer stopProcess(t, cmdA)
	cmdB, outB := startProcessWithOutput(t, runB, gatewayBin, nil, []string{
		"GATEWAY_ADDR=" + addrB,
		"GATEWAY_AGENT_PUBLIC_KEY=" + b.AgentPublicKey,
		"GATEWAY_API_KEYS_JSON=" + b.APIKeysJSON,
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
		"GATEWAY_RATE_LIMIT_BURST=100",
	})
	defer stopProcess(t, cmdB)
	baseA := "http://" + addrA
	baseB := "http://" + addrB
	waitForHTTP(t, baseA+"/healthz", "", http.StatusOK)
	waitForHTTP(t, baseB+"/healthz", "", http.StatusOK)

	connA := dialManualAgent(t, "ws://"+addrA+"/ws/agent", a.PrivateKey)
	defer connA.Close()
	serveMetaOnce(t, connA, openAPIDocFor("tenant_a_capability"))
	connB := dialManualAgent(t, "ws://"+addrB+"/ws/agent", b.PrivateKey)
	defer connB.Close()
	serveMetaOnce(t, connB, openAPIDocFor("tenant_b_capability"))
	waitForHTTP(t, baseA+"/openapi.json", a.APIKey, http.StatusOK)
	waitForHTTP(t, baseB+"/openapi.json", b.APIKey, http.StatusOK)

	if status, body := getStatusWithAPIKey(t, baseA+"/openapi.json", b.APIKey); status != http.StatusUnauthorized {
		t.Fatalf("tenant B key on tenant A status=%d body=%s", status, string(body))
	}
	docA := string(getWithAPIKey(t, baseA+"/openapi.json", a.APIKey))
	docB := string(getWithAPIKey(t, baseB+"/openapi.json", b.APIKey))
	if !strings.Contains(docA, "tenant_a_capability") || strings.Contains(docA, "tenant_b_capability") {
		t.Fatalf("tenant A OpenAPI leaked state: %s", docA)
	}
	if !strings.Contains(docB, "tenant_b_capability") || strings.Contains(docB, "tenant_a_capability") {
		t.Fatalf("tenant B OpenAPI leaked state: %s", docB)
	}
	if strings.Contains(outA.String(), "tenant_b_capability") || strings.Contains(outB.String(), "tenant_a_capability") {
		t.Fatalf("tenant logs leaked state: A=%s B=%s", outA.String(), outB.String())
	}
}

func TestGatewayProcessDirectHTTPIgnoresForwardedHeaders(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	gatewayBin := filepath.Join(tmp, "onprest-gateway")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	cmd, output := startProcessWithOutput(t, tmp, gatewayBin, nil, []string{
		"GATEWAY_ADDR=" + addr,
		"GATEWAY_AGENT_PUBLIC_KEY=" + secrets.AgentPublicKey,
		"GATEWAY_API_KEYS_JSON=" + secrets.APIKeysJSON,
		"GATEWAY_IP_ALLOW_LIST=127.0.0.1/32",
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
		"GATEWAY_RATE_LIMIT_BURST=100",
	})
	defer stopProcess(t, cmd)
	waitForHTTP(t, "http://"+addr+"/healthz", "", http.StatusOK)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secrets.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "198.51.100.10")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct process status=%d body=%s output=%s", resp.StatusCode, string(body), output.String())
	}
}

func TestOperationalSensitiveDataAuditAcrossArtifacts(t *testing.T) {
	db := postgresContainerConfig(t)
	repo := repoRoot(t)
	tmp := t.TempDir()
	gatewayBin := filepath.Join(tmp, "onprest-gateway")
	agentBin := filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	secrets := newITSecrets(t)
	addr := freeAddr(t)
	const sqlMarker = "onprest_audit_sql_marker"
	const paramSecret = "audit-secret-param"
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, `  audited_failure:
    sql: select :label::text as label, (1 / :denominator::int)::int as value /* onprest_audit_sql_marker */
    params:
      label:
        type: string
        required: true
      denominator:
        type: integer
        required: true
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      label:
        type: string
      value:
        type: integer`)

	gatewayCmd, gatewayOut := startProcessWithOutput(t, tmp, gatewayBin, nil, []string{
		"GATEWAY_ADDR=" + addr,
		"GATEWAY_AGENT_PUBLIC_KEY=" + secrets.AgentPublicKey,
		"GATEWAY_API_KEYS_JSON=" + secrets.APIKeysJSON,
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
		"GATEWAY_RATE_LIMIT_BURST=100",
	})
	defer stopProcess(t, gatewayCmd)
	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)
	agentCmd, agentOut := startProcessWithOutput(t, tmp, agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + capabilityFile})
	defer stopProcess(t, agentCmd)
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

	openAPI := string(getWithAPIKey(t, baseURL+"/openapi.json", secrets.APIKey))
	tools := string(postMCPPayload(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	status, body := postCapability(t, baseURL, secrets.APIKey, "audited_failure", `{"label":"`+paramSecret+`","denominator":0}`)
	if status != http.StatusBadGateway {
		t.Fatalf("audited_failure status=%d body=%s", status, string(body))
	}
	requireAPIErrorCode(t, body, "AGENT_QUERY_FAILED")

	gatewayArtifacts := map[string]string{
		"gateway stdout": gatewayOut.String(),
		"HTTP response":  string(body),
		"OpenAPI":        openAPI,
		"MCP tools/list": tools,
	}
	for name, artifact := range gatewayArtifacts {
		if strings.Contains(artifact, paramSecret) ||
			strings.Contains(artifact, sqlMarker) ||
			strings.Contains(artifact, db.Password) ||
			strings.Contains(strings.ToLower(artifact), "division") {
			t.Fatalf("%s leaked sensitive detail: %s", name, artifact)
		}
	}
	if strings.Contains(agentOut.String(), paramSecret) ||
		strings.Contains(agentOut.String(), sqlMarker) ||
		strings.Contains(agentOut.String(), db.Password) ||
		strings.Contains(strings.ToLower(agentOut.String()), "division") {
		t.Fatalf("agent stdout leaked sensitive detail: %s", agentOut.String())
	}
	assertJSONLinesCollectorCompatible(t, gatewayOut.String())
	assertJSONLinesCollectorCompatible(t, agentOut.String())

	detailLog, err := os.ReadFile(agentBin + ".log")
	if err != nil {
		t.Fatalf("read agent local detail log: %v", err)
	}
	if !bytes.Contains(detailLog, []byte("AGENT_QUERY_FAILED")) ||
		!bytes.Contains(bytes.ToLower(detailLog), []byte("division")) {
		t.Fatalf("agent local detail log missing DB detail: %s", string(detailLog))
	}
	if bytes.Contains(detailLog, []byte(db.Password)) {
		t.Fatalf("agent local detail log leaked DB password: %s", string(detailLog))
	}
}

func TestOperationalAgentSecretRotationWithRealBinaries(t *testing.T) {
	db := postgresContainerConfig(t)
	repo := repoRoot(t)
	tmp := t.TempDir()
	gatewayBin := filepath.Join(tmp, "onprest-gateway")
	agentBin := filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	addr := freeAddr(t)
	oldSecrets := newITSecrets(t)
	oldCapability := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", oldSecrets.AgentPrivateKey, capabilityBlock("old_capability", "select 1::int as id"))
	oldGateway, _ := startProcessWithOutput(t, tmp, gatewayBin, nil, []string{
		"GATEWAY_ADDR=" + addr,
		"GATEWAY_AGENT_PUBLIC_KEY=" + oldSecrets.AgentPublicKey,
		"GATEWAY_API_KEYS_JSON=" + oldSecrets.APIKeysJSON,
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
		"GATEWAY_RATE_LIMIT_BURST=100",
	})
	defer stopProcess(t, oldGateway)
	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)
	oldAgent, _ := startProcessWithOutput(t, tmp, agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + oldCapability})
	defer stopProcess(t, oldAgent)
	waitForHTTP(t, baseURL+"/openapi.json", oldSecrets.APIKey, http.StatusOK)
	status, body := postCapability(t, baseURL, oldSecrets.APIKey, "old_capability", `{}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"id":1`) {
		t.Fatalf("old_capability status=%d body=%s", status, string(body))
	}

	stopProcess(t, oldAgent)
	stopProcess(t, oldGateway)

	newSecrets := newITSecrets(t)
	newCapability := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", newSecrets.AgentPrivateKey, capabilityBlock("new_capability", "select 2::int as id"))
	newGateway, newGatewayOut := startProcessWithOutput(t, tmp, gatewayBin, nil, []string{
		"GATEWAY_ADDR=" + addr,
		"GATEWAY_AGENT_PUBLIC_KEY=" + newSecrets.AgentPublicKey,
		"GATEWAY_API_KEYS_JSON=" + newSecrets.APIKeysJSON,
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
		"GATEWAY_RATE_LIMIT_BURST=100",
	})
	defer stopProcess(t, newGateway)
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)
	if conn, err := wsDialForError(t, "ws://"+addr+"/ws/agent", oldSecrets.PrivateKey); err == nil {
		_ = conn.Close()
		t.Fatal("old agent private key connected after gateway public key rotation")
	}
	newAgent, newAgentOut := startProcessWithOutput(t, tmp, agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + newCapability})
	defer stopProcess(t, newAgent)
	waitForHTTP(t, baseURL+"/openapi.json", newSecrets.APIKey, http.StatusOK)
	openAPI := string(getWithAPIKey(t, baseURL+"/openapi.json", newSecrets.APIKey))
	if !strings.Contains(openAPI, "new_capability") || strings.Contains(openAPI, "old_capability") {
		t.Fatalf("rotated OpenAPI metadata = %s\ngateway=%s\nagent=%s", openAPI, newGatewayOut.String(), newAgentOut.String())
	}
	status, body = postCapability(t, baseURL, newSecrets.APIKey, "new_capability", `{}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"id":2`) {
		t.Fatalf("new_capability status=%d body=%s", status, string(body))
	}
}

func assertJSONLinesCollectorCompatible(t *testing.T, raw string) {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(strings.TrimSpace(raw)))
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		count++
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("collector could not parse JSON line %q: %v\nall output:\n%s", line, err, raw)
		}
		if _, ok := event["event"]; !ok {
			t.Fatalf("collector event missing event field: %s", line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan JSON lines: %v", err)
	}
	if count == 0 {
		t.Fatalf("collector received no JSON lines")
	}
}

func runBinaryJSON(t *testing.T, dir, bin string, args []string, out any) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s failed: %v\nstdout=%s\nstderr=%s", filepath.Base(bin), strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("%s %s stderr: %s", filepath.Base(bin), strings.Join(args, " "), stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		t.Fatalf("decode %s %s output: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, stdout.String())
	}
}
