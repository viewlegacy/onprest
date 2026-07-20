//go:build integration

package it

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/ws"
)

func TestBuiltGatewayRunsOutsideSourceTree(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	gatewayBin := filepath.Join(tmp, "standalone-gateway")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")

	secrets := newITSecrets(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDir := filepath.Join(tmp, "run")
	if err := os.Mkdir(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := startProcessInDir(t, ctx, runDir, gatewayBin, nil, []string{
		"GATEWAY_ADDR=" + addr,
		"GATEWAY_AGENT_PUBLIC_KEY=" + secrets.AgentPublicKey,
		"GATEWAY_API_KEYS_JSON=" + secrets.APIKeysJSON,
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=50",
		"GATEWAY_RATE_LIMIT_BURST=50",
	})
	defer stopProcess(t, cmd)

	waitForHTTP(t, "http://"+addr+"/healthz", "", http.StatusOK)
}

func TestGatewayProcessAppliesEnvRequestBodyLimit(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	gatewayBin := filepath.Join(tmp, "onprest-gateway")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")

	secrets := newITSecrets(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := startProcessInDir(t, ctx, tmp, gatewayBin, nil, []string{
		"GATEWAY_ADDR=" + addr,
		"GATEWAY_AGENT_PUBLIC_KEY=" + secrets.AgentPublicKey,
		"GATEWAY_API_KEYS_JSON=" + secrets.APIKeysJSON,
		"GATEWAY_MAX_REQUEST_BODY_BYTES=96",
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=50",
		"GATEWAY_RATE_LIMIT_BURST=50",
	})
	defer stopProcess(t, cmd)
	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)

	restPrefix := `{"padding":"`
	restSuffix := `"}`
	restExactBody := restPrefix + strings.Repeat("x", 96-len(restPrefix)-len(restSuffix)) + restSuffix
	if len(restExactBody) != 96 {
		t.Fatalf("REST exact body length = %d, want 96", len(restExactBody))
	}
	if status, body := postCapability(t, baseURL, secrets.APIKey, "offline_capability", restExactBody); status != http.StatusServiceUnavailable {
		t.Fatalf("REST exact-limit status=%d, want 503 proving request parsing passed; body=%s", status, body)
	}
	if status, body := postCapability(t, baseURL, secrets.APIKey, "offline_capability", restExactBody+" "); status != http.StatusBadRequest {
		t.Fatalf("REST over-limit status=%d, want 400; body=%s", status, body)
	} else {
		requireAPIErrorCode(t, body, "GATEWAY_INVALID_REQUEST")
	}

	prefix := `{"jsonrpc":"2.0","id":1,"method":"ping","padding":"`
	suffix := `"}`
	exactBody := prefix + strings.Repeat("x", 96-len(prefix)-len(suffix)) + suffix
	if len(exactBody) != 96 {
		t.Fatalf("exact body length = %d, want 96", len(exactBody))
	}
	if body := postMCPPayload(t, baseURL, secrets.APIKey, exactBody); !strings.Contains(string(body), `"result"`) {
		t.Fatalf("exact-limit MCP request was not accepted: %s", body)
	}
	if body := postMCPPayload(t, baseURL, secrets.APIKey, exactBody+" "); !strings.Contains(string(body), `"code":-32700`) || !strings.Contains(string(body), `"code":"PARSE_ERROR"`) {
		t.Fatalf("over-limit MCP request did not return parse error: %s", body)
	}
}

func TestGatewayReturnsTimeoutWhenConnectedAgentDoesNotRespond(t *testing.T) {
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, 20*time.Millisecond)

	conn := dialManualAgent(t, "ws://"+addr+"/ws/agent", secrets.PrivateKey)
	defer conn.Close()
	go func() {
		for {
			if _, err := conn.ReadText(); err != nil {
				return
			}
		}
	}()

	waitForAgentConnected(t, baseURL)
	status, body := postCapability(t, baseURL, secrets.APIKey, "echo_customer", `{"id":1}`)
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d; body=%s", status, http.StatusGatewayTimeout, string(body))
	}
	requireAPIErrorCode(t, body, "GATEWAY_TIMEOUT")
}

func TestGatewayRejectsDuplicateAgentAndAcceptsReconnect(t *testing.T) {
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &bytes.Buffer{}
	baseURL := startInternalGatewayWithLog(t, ctx, addr, secrets, 100*time.Millisecond, logs)
	gatewayURL := "ws://" + addr + "/ws/agent"

	first := dialManualAgent(t, gatewayURL, secrets.PrivateKey)
	defer first.Close()
	waitForAgentConnected(t, baseURL)

	_, err := wsDialForError(t, gatewayURL, secrets.PrivateKey)
	if err == nil || !strings.Contains(err.Error(), "409 Conflict") {
		t.Fatalf("duplicate agent dial error = %v, want 409 Conflict", err)
	}
	if got := logs.String(); !strings.Contains(got, "agent_rejected") || !strings.Contains(got, "already_connected") {
		t.Fatalf("duplicate agent warning log missing: %s", got)
	}

	_ = first.Close()
	waitForAgentDisconnected(t, baseURL)
	second := dialManualAgent(t, gatewayURL, secrets.PrivateKey)
	defer second.Close()
	waitForAgentConnected(t, baseURL)
}

func waitForAgentConnected(t *testing.T, baseURL string) {
	t.Helper()
	waitForHealthAgentState(t, baseURL, true)
}

func waitForAgentDisconnected(t *testing.T, baseURL string) {
	t.Helper()
	waitForHealthAgentState(t, baseURL, false)
}

func waitForHealthAgentState(t *testing.T, baseURL string, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			var body struct {
				AgentConnected bool `json:"agent_connected"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && body.AgentConnected == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("agent_connected did not become %t", want)
}

func wsDialForError(t *testing.T, gatewayURL string, privateKey ed25519.PrivateKey) (*ws.Conn, error) {
	t.Helper()
	return ws.Dial(2*time.Second, gatewayURL, signedAgentHeaders(t, gatewayURL, privateKey, "/ws/agent"))
}
