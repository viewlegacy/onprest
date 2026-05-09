//go:build integration

package it

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestGatewayAgentPostgresIntegration(t *testing.T) {
	db := postgresContainerConfig(t)
	repo := repoRoot(t)
	tmp := t.TempDir()
	gatewayBin := filepath.Join(tmp, "onprest-gateway")
	agentBin := filepath.Join(tmp, "onprest-agent")

	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	agentPrivateKey := base64.RawURLEncoding.EncodeToString(privateKey)
	agentPublicKey := base64.RawURLEncoding.EncodeToString(publicKey)
	apiKey := "onprest-it-api-key"
	apiKeyHash, err := bcrypt.GenerateFromPassword([]byte(apiKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	apiKeysJSON, err := json.Marshal([]map[string]any{{
		"name":         "it",
		"key_hash":     string(apiKeyHash),
		"capabilities": []string{"*"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	addr := freeAddr(t)
	baseURL := "http://" + addr
	gatewayURL := "ws://" + addr + "/ws/agent"
	capabilityFile := renderCapability(t, repo, tmp, db, gatewayURL, agentPrivateKey)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gateway := startProcess(t, ctx, gatewayBin, nil, []string{
		"GATEWAY_ADDR=" + addr,
		"GATEWAY_AGENT_PUBLIC_KEY=" + agentPublicKey,
		"GATEWAY_API_KEYS_JSON=" + string(apiKeysJSON),
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=50",
		"GATEWAY_RATE_LIMIT_BURST=50",
	})
	defer stopProcess(t, gateway)
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)

	agent := startProcess(t, ctx, agentBin, nil, []string{
		"AGENT_CAPABILITY_FILE=" + capabilityFile,
	})
	defer stopProcess(t, agent)

	waitForHTTP(t, baseURL+"/openapi.json", apiKey, http.StatusOK)
	assertRESTCapability(t, baseURL, apiKey)
	assertMCPTools(t, baseURL, apiKey)
}

type postgresConfig struct {
	Host     string
	Port     string
	Name     string
	User     string
	Password string
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
}

func buildBinary(t *testing.T, repo, out, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, pkg)
	cmd.Dir = repo
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build %s failed: %v\n%s", pkg, err, stderr.String())
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			if os.Getenv("ONPREST_IT_REQUIRE_CONTAINERS") == "1" {
				t.Fatalf("local TCP listen is required in strict integration mode: %v", err)
			}
			t.Skipf("local TCP listen is not permitted in this environment: %v", err)
		}
		t.Fatalf("listen for free addr: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func renderCapability(t *testing.T, repo, tmp string, db postgresConfig, gatewayURL, agentPrivateKey string) string {
	t.Helper()
	templatePath := filepath.Join(repo, "it", "testdata", "capability.postgres.yaml.tmpl")
	b, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("read capability template: %v", err)
	}
	replacements := map[string]string{
		"DB_HOST":           yamlString(db.Host),
		"DB_PORT":           db.Port,
		"DB_NAME":           yamlString(db.Name),
		"DB_USER":           yamlString(db.User),
		"DB_PASSWORD":       yamlString(db.Password),
		"GATEWAY_URL":       yamlString(gatewayURL),
		"AGENT_PRIVATE_KEY": yamlString(agentPrivateKey),
	}
	content := string(b)
	for key, value := range replacements {
		content = strings.ReplaceAll(content, "{{"+key+"}}", value)
	}
	path := filepath.Join(tmp, "capability.postgres.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write rendered capability: %v", err)
	}
	return path
}

func yamlString(v string) string {
	return strconv.Quote(v)
}

func startProcess(t *testing.T, ctx context.Context, bin string, args []string, env []string) *exec.Cmd {
	t.Helper()
	return startProcessInDir(t, ctx, "", bin, args, env)
}

func startProcessInDir(t *testing.T, ctx context.Context, dir, bin string, args []string, env []string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), env...)
	var output lockedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", filepath.Base(bin), err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s output:\n%s", filepath.Base(bin), output.String())
		}
	})
	return cmd
}

func stopProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func waitForHTTP(t *testing.T, url, apiKey string, wantStatus int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
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
			last = fmt.Sprintf("status=%d body=%s", resp.StatusCode, string(body))
		} else {
			last = err.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %s", url, last)
}

func assertRESTCapability(t *testing.T, baseURL, apiKey string) {
	t.Helper()
	body := strings.NewReader(`{"id":7,"name":"Ada"}`)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/capabilities/echo_customer", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("REST status=%d body=%s", resp.StatusCode, string(b))
	}
	var got struct {
		Rows  []map[string]any `json:"rows"`
		Count int              `json:"count"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 1 || len(got.Rows) != 1 || got.Rows[0]["name"] != "Ada" || got.Rows[0]["id"] != float64(7) {
		t.Fatalf("unexpected REST response: %s", string(b))
	}
}

func assertMCPTools(t *testing.T, baseURL, apiKey string) {
	t.Helper()
	callMCP(t, baseURL, apiKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, func(body []byte) {
		if !strings.Contains(string(body), "echo_customer") {
			t.Fatalf("tools/list missing echo_customer: %s", string(body))
		}
	})
	callMCP(t, baseURL, apiKey, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo_customer","arguments":{"id":8,"name":"Grace"}}}`, func(body []byte) {
		if !strings.Contains(string(body), `"count":1`) || !strings.Contains(string(body), "Grace") {
			t.Fatalf("unexpected tools/call response: %s", string(body))
		}
	})
}

func callMCP(t *testing.T, baseURL, apiKey, payload string, assert func([]byte)) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/mcp", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("MCP status=%d body=%s", resp.StatusCode, string(body))
	}
	assert(body)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
