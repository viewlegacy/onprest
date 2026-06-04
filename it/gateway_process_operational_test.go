//go:build integration

package it

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGatewayProcessEnvFileMCPLogsAndShutdown(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	gatewayBin := filepath.Join(tmp, "onprest-gateway")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")

	secrets := newITSecrets(t)
	addr := freeAddr(t)
	envFile := filepath.Join(tmp, "gateway.env")
	writeFile(t, envFile, "GATEWAY_ADDR="+addr+"\n"+
		"GATEWAY_AGENT_PUBLIC_KEY="+secrets.AgentPublicKey+"\n"+
		"GATEWAY_API_KEYS_JSON='[{\"name\":\"it\",\"key_hash\":\""+secrets.APIKeyHash+"\",\"capabilities\":[\"*\"]}]'\n"+
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100\n"+
		"GATEWAY_RATE_LIMIT_BURST=100\n")

	cmd, output := startShellProcessWithOutput(t, tmp, "set -a; . "+shellQuote(envFile)+"; set +a; exec "+shellQuote(gatewayBin))
	waitForHTTP(t, "http://"+addr+"/healthz", "", http.StatusOK)

	for _, payload := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	} {
		body := postMCPPayload(t, "http://"+addr, secrets.APIKey, payload)
		if !strings.Contains(string(body), `"result"`) {
			t.Fatalf("MCP response missing result: %s", string(body))
		}
	}
	status, body := postCapability(t, "http://"+addr, secrets.APIKey, "echo_customer", `{"secret":"must-not-log"}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("offline REST status=%d body=%s", status, string(body))
	}
	requireAPIErrorCode(t, body, "GATEWAY_AGENT_OFFLINE")

	_ = cmd.Process.Signal(os.Interrupt)
	if err := waitForExit(t, cmd, 5*time.Second); err != nil {
		t.Fatalf("gateway did not exit cleanly: %v\n%s", err, output.String())
	}
	events := parseJSONLines(t, output.String())
	if len(events) == 0 {
		t.Fatalf("no gateway logs captured")
	}
	foundStart := false
	foundREST := false
	for _, event := range events {
		switch event["event"] {
		case "gateway_start":
			foundStart = true
		case "request":
			if event["capability"] == "echo_customer" && event["http_status"] == float64(http.StatusServiceUnavailable) {
				foundREST = true
			}
		}
	}
	if !foundStart || !foundREST {
		t.Fatalf("missing gateway_start or REST request log: %s", output.String())
	}
	if strings.Contains(output.String(), "must-not-log") {
		t.Fatalf("gateway stdout leaked params: %s", output.String())
	}
}

func TestGatewayProcessTrustedProxyIPAllowAndRateLimit(t *testing.T) {
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
		"GATEWAY_TRUSTED_PROXY_CIDRS=127.0.0.1/32",
		"GATEWAY_IP_ALLOW_LIST=203.0.113.9/32",
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=0.1",
		"GATEWAY_RATE_LIMIT_BURST=1",
	})
	defer stopProcess(t, cmd)
	waitForHTTPStatusWithHeaders(t, "http://"+addr+"/healthz", http.StatusOK, map[string]string{"X-Forwarded-For": "203.0.113.9"})
	waitForHTTPStatusWithHeaders(t, "http://"+addr+"/healthz", http.StatusForbidden, map[string]string{"X-Forwarded-For": "198.51.100.10"})
	waitForHTTPStatusWithHeaders(t, "http://"+addr+"/healthz", http.StatusTooManyRequests, map[string]string{"X-Forwarded-For": "203.0.113.9"})
	waitForOutputContains(t, output, "GATEWAY_IP_DENIED", "GATEWAY_RATE_LIMITED")
}

func TestGatewayDistLikeBinariesRunOutsideSourceTree(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	gatewayBin := filepath.Join(tmp, "onprest-gateway")
	agentBin := filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")

	secrets := newITSecrets(t)
	addr := freeAddr(t)
	runDir := filepath.Join(tmp, "run")
	if err := os.Mkdir(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gatewayCmd, _ := startProcessWithOutput(t, runDir, gatewayBin, nil, []string{
		"GATEWAY_ADDR=" + addr,
		"GATEWAY_AGENT_PUBLIC_KEY=" + secrets.AgentPublicKey,
		"GATEWAY_API_KEYS_JSON=" + secrets.APIKeysJSON,
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
		"GATEWAY_RATE_LIMIT_BURST=100",
	})
	defer stopProcess(t, gatewayCmd)
	waitForHTTP(t, "http://"+addr+"/healthz", "", http.StatusOK)

	cmd := exec.Command(agentBin)
	cmd.Dir = runDir
	cmd.Env = append(os.Environ(), "AGENT_CAPABILITY_FILE="+filepath.Join(runDir, "missing.yaml"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("agent with missing repo-external capability file succeeded, want failure proving binary executed")
	}
}

func TestGatewayProcessRollingShutdownDuringInFlightRequest(t *testing.T) {
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
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
		"GATEWAY_RATE_LIMIT_BURST=100",
	})
	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)
	conn := dialManualAgent(t, "ws://"+addr+"/ws/agent", secrets.PrivateKey)
	defer conn.Close()
	serveMetaOnce(t, conn, openAPIDocFor("slow_capability"))
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

	done := make(chan struct{}, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/capabilities/slow_capability", strings.NewReader(`{}`))
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+secrets.APIKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}
		}
		done <- struct{}{}
	}()
	_ = readAgentRequest(t, conn, "slow_capability")
	_ = cmd.Process.Signal(os.Interrupt)
	if err := waitForExit(t, cmd, 12*time.Second); err != nil {
		t.Fatalf("gateway did not exit cleanly during in-flight request: %v\n%s", err, output.String())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight REST client did not return after gateway shutdown")
	}
}

func startProcessWithOutput(t *testing.T, dir, bin string, args []string, env []string) (*exec.Cmd, *lockedBuffer) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), env...)
	output := &lockedBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", filepath.Base(bin), err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s output:\n%s", filepath.Base(bin), output.String())
		}
	})
	return cmd, output
}

func startShellProcessWithOutput(t *testing.T, dir, script string) (*exec.Cmd, *lockedBuffer) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	if dir != "" {
		cmd.Dir = dir
	}
	output := &lockedBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start shell process: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("shell output:\n%s", output.String())
		}
	})
	return cmd, output
}

func waitForHTTPStatusWithHeaders(t *testing.T, rawURL string, want int, headers map[string]string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
			last = string(body)
		} else {
			last = err.Error()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s status %d: %s", rawURL, want, last)
}

func waitForOutputContains(t *testing.T, output *lockedBuffer, needles ...string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		raw := output.String()
		foundAll := true
		for _, needle := range needles {
			if !strings.Contains(raw, needle) {
				foundAll = false
				break
			}
		}
		if foundAll {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected output to contain %s, got: %s", strings.Join(needles, ", "), output.String())
}

func parseJSONLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	var out []map[string]any
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("parse JSON line %q: %v\nall output:\n%s", line, err, raw)
		}
		out = append(out, entry)
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
