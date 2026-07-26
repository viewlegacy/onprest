//go:build integration

package it

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/gateway"
	"golang.org/x/crypto/bcrypt"
)

func TestGatewayWorksThroughGenericReverseProxyForHTTPAndWebSocket(t *testing.T) {
	secrets := newITSecrets(t)
	_, trustedProxyCIDR, err := net.ParseCIDR("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	_, allowedClientCIDR, err := net.ParseCIDR("203.0.113.9/32")
	if err != nil {
		t.Fatal(err)
	}
	backendAddr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := gateway.NewServer(gateway.Config{
		Addr:           backendAddr,
		AgentPublicKey: secrets.AgentPublicKey,
		APIKeys: []gateway.APIKey{{
			Name:         "it",
			KeyHash:      secrets.APIKeyHash,
			Capabilities: []string{"*"},
		}},
		TrustedProxies: []*net.IPNet{trustedProxyCIDR},
		IPAllowList:    []*net.IPNet{allowedClientCIDR},
		RateLimit:      gateway.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
		AgentTimeout:   time.Second,
	}, io.Discard)
	go func() { _ = srv.ListenAndServeContext(ctx) }()
	waitForHTTPStatusWithHeaders(t, "http://"+backendAddr+"/healthz", http.StatusOK, map[string]string{"X-Forwarded-For": "203.0.113.9"})

	proxy := startReverseProxy(t, "http://"+backendAddr, "203.0.113.9")
	defer proxy.Close()
	proxyURL := strings.TrimPrefix(proxy.URL, "http://")

	conn := dialManualAgent(t, "ws://"+proxyURL+"/ws/agent", secrets.PrivateKey)
	defer conn.Close()
	serveMetaOnce(t, conn, openAPIDocFor("echo_customer"))
	waitForHTTP(t, proxy.URL+"/openapi.json", secrets.APIKey, http.StatusOK)

	openAPI := getWithAPIKey(t, proxy.URL+"/openapi.json", secrets.APIKey)
	if !strings.Contains(string(openAPI), "echo_customer") {
		t.Fatalf("OpenAPI through proxy missing capability: %s", string(openAPI))
	}
	tools := postMCPPayload(t, proxy.URL, secrets.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(string(tools), "echo_customer") {
		t.Fatalf("tools/list through proxy missing capability: %s", string(tools))
	}

	done := make(chan []byte, 1)
	go func() {
		status, body := postCapability(t, proxy.URL, secrets.APIKey, "echo_customer", `{"id":9}`)
		if status != http.StatusOK {
			body = []byte("status=" + http.StatusText(status) + " body=" + string(body))
		}
		done <- body
	}()
	req := readAgentRequest(t, conn, "echo_customer")
	if err := conn.WriteText(resultForRequest(req.ID, map[string]any{"rows": []any{map[string]any{"id": 9}}, "count": 1})); err != nil {
		t.Fatalf("write proxy REST response: %v", err)
	}
	body := <-done
	if !strings.Contains(string(body), `"count":1`) {
		t.Fatalf("REST through proxy body=%s", string(body))
	}
}

func TestDirectHTTPIgnoresForwardedHeadersWhenNoTrustedProxy(t *testing.T) {
	secrets := newITSecrets(t)
	_, loopbackCIDR, err := net.ParseCIDR("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := gateway.NewServer(gateway.Config{
		Addr:           addr,
		AgentPublicKey: secrets.AgentPublicKey,
		APIKeys: []gateway.APIKey{{
			Name:         "it",
			KeyHash:      secrets.APIKeyHash,
			Capabilities: []string{"*"},
		}},
		IPAllowList:  []*net.IPNet{loopbackCIDR},
		RateLimit:    gateway.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
		AgentTimeout: time.Second,
	}, io.Discard)
	go func() { _ = srv.ListenAndServeContext(ctx) }()
	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
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
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}
}

func TestGatewayCacheUpdatesWhenAgentReconnects(t *testing.T) {
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, time.Second)

	first := dialManualAgent(t, "ws://"+addr+"/ws/agent", secrets.PrivateKey)
	serveMetaOnce(t, first, openAPIDocFor("first_capability"))
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)
	if got := string(getWithAPIKey(t, baseURL+"/openapi.json", secrets.APIKey)); !strings.Contains(got, "first_capability") {
		t.Fatalf("first OpenAPI = %s", got)
	}
	_ = first.Close()
	waitForAgentDisconnected(t, baseURL)
	if status, body := getWithAPIKeyStatus(t, baseURL+"/openapi.json", secrets.APIKey); status != http.StatusServiceUnavailable || strings.Contains(string(body), "first_capability") {
		t.Fatalf("disconnected OpenAPI status=%d body=%s", status, string(body))
	}

	second := dialManualAgent(t, "ws://"+addr+"/ws/agent", secrets.PrivateKey)
	defer second.Close()
	serveMetaOnce(t, second, openAPIDocFor("second_capability"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, body := getWithAPIKeyStatus(t, baseURL+"/openapi.json", secrets.APIKey)
		got := string(body)
		if status != http.StatusOK {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if strings.Contains(got, "second_capability") && !strings.Contains(got, "first_capability") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("OpenAPI cache did not update to second agent metadata")
}

func TestGatewaySecretRotationRejectsOldAgentKey(t *testing.T) {
	oldSecrets := newITSecrets(t)
	newSecrets := newITSecrets(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := gateway.NewServer(gateway.Config{
		Addr:           addr,
		AgentPublicKey: newSecrets.AgentPublicKey,
		APIKeys: []gateway.APIKey{{
			Name:         "it",
			KeyHash:      newSecrets.APIKeyHash,
			Capabilities: []string{"*"},
		}},
		RateLimit:    gateway.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
		AgentTimeout: time.Second,
	}, io.Discard)
	go func() { _ = srv.ListenAndServeContext(ctx) }()
	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)

	if _, err := wsDialForError(t, "ws://"+addr+"/ws/agent", oldSecrets.PrivateKey); err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("old key dial error = %v, want 401 Unauthorized", err)
	}
	conn := dialManualAgent(t, "ws://"+addr+"/ws/agent", newSecrets.PrivateKey)
	defer conn.Close()
	serveMetaOnce(t, conn, openAPIDocFor("rotated_capability"))
	waitForHTTP(t, baseURL+"/openapi.json", newSecrets.APIKey, http.StatusOK)
}

func TestDedicatedGatewayInstancesDoNotShareState(t *testing.T) {
	a := newITSecrets(t)
	b := newITSecrets(t)
	b.APIKey = "tenant-b-api-key"
	hash, err := bcrypt.GenerateFromPassword([]byte(b.APIKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	b.APIKeyHash = string(hash)
	b.APIKeysJSON = `[{"name":"it","key_hash":` + quoteJSON(b.APIKeyHash) + `,"capabilities":["*"]}]`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrA := freeAddr(t)
	addrB := freeAddr(t)
	baseA := startInternalGateway(t, ctx, addrA, a, time.Second)
	baseB := startInternalGateway(t, ctx, addrB, b, time.Second)

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
}

func startReverseProxy(t *testing.T, target string, forwardedFor string) *httptest.Server {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)
		r.Header.Set("X-Forwarded-For", forwardedFor)
	}
	return httptest.NewServer(proxy)
}

func readAgentRequest(t *testing.T, conn interface{ ReadText() ([]byte, error) }, capability string) protocolRequest {
	t.Helper()
	msg, err := conn.ReadText()
	if err != nil {
		t.Fatalf("read agent request: %v", err)
	}
	var req protocolRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		t.Fatalf("decode agent request: %v; msg=%s", err, string(msg))
	}
	if req.Capability != capability {
		t.Fatalf("capability=%q, want %q", req.Capability, capability)
	}
	return req
}

type protocolRequest struct {
	ID         string         `json:"id"`
	Capability string         `json:"capability"`
	Params     map[string]any `json:"params"`
}

func resultForRequest(id string, result any) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "result": result})
	return b
}

func getStatusWithAPIKey(t *testing.T, url, apiKey string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}
