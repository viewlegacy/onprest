//go:build integration

package it

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/gateway"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
	"golang.org/x/crypto/bcrypt"
)

func TestCLIIssuedSecretsAuthenticateGateway(t *testing.T) {
	var agentOut, keyOut, stderr bytes.Buffer
	gateway.HandleCLI([]string{"create-agent-secret"}, &agentOut, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("create-agent-secret stderr: %s", stderr.String())
	}
	gateway.HandleCLI([]string{"create-key", "--name", "limited", "--capabilities", "echo_customer"}, &keyOut, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("create-key stderr: %s", stderr.String())
	}

	var agentKeys struct {
		PublicKey  string `json:"agent_public_key"`
		PrivateKey string `json:"agent_private_key"`
	}
	if err := json.Unmarshal(agentOut.Bytes(), &agentKeys); err != nil {
		t.Fatalf("decode agent keys: %v", err)
	}
	var key struct {
		Name         string          `json:"name"`
		APIKey       string          `json:"api_key"`
		KeyHash      string          `json:"key_hash"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(keyOut.Bytes(), &key); err != nil {
		t.Fatalf("decode api key: %v", err)
	}

	addr := freeAddr(t)
	t.Setenv("GATEWAY_ADDR", addr)
	t.Setenv("GATEWAY_AGENT_PUBLIC_KEY", agentKeys.PublicKey)
	t.Setenv("GATEWAY_API_KEYS_JSON", `[{"name":`+quoteJSON(key.Name)+`,"key_hash":`+quoteJSON(key.KeyHash)+`,"capabilities":`+string(key.Capabilities)+`}]`)
	t.Setenv("GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND", "100")
	t.Setenv("GATEWAY_RATE_LIMIT_BURST", "100")
	cfg, err := gateway.LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	cfg.AgentTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := gateway.NewServer(cfg, io.Discard)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServeContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("gateway stopped: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("gateway did not stop")
		}
	})
	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)

	privateKey := decodePrivateKey(t, agentKeys.PrivateKey)
	conn := dialManualAgent(t, "ws://"+addr+"/ws/agent", privateKey)
	defer conn.Close()
	serveMetaOnce(t, conn, openAPIDocFor("echo_customer", "hidden_customer"))
	waitForHTTP(t, baseURL+"/openapi.json", key.APIKey, http.StatusOK)
	tools := postMCPPayload(t, baseURL, key.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(string(tools), "echo_customer") || strings.Contains(string(tools), "hidden_customer") {
		t.Fatalf("CLI issued key tools/list = %s", string(tools))
	}

	status, body := postCapability(t, baseURL, key.APIKey, "hidden_customer", `{}`)
	if status != http.StatusForbidden {
		t.Fatalf("hidden status = %d, want 403; body=%s", status, string(body))
	}
	requireAPIErrorCode(t, body, "GATEWAY_CAPABILITY_DENIED")

	done := make(chan []byte, 1)
	go func() {
		status, body := postCapability(t, baseURL, key.APIKey, "echo_customer", `{"id":11}`)
		if status != http.StatusOK {
			body = []byte("status=" + http.StatusText(status) + " body=" + string(body))
		}
		done <- body
	}()
	msg, err := conn.ReadText()
	if err != nil {
		t.Fatalf("read CLI issued key capability request: %v", err)
	}
	var req protocol.Request
	if err := json.Unmarshal(msg, &req); err != nil {
		t.Fatalf("decode CLI issued key capability request: %v; msg=%s", err, string(msg))
	}
	if req.Capability != "echo_customer" || req.ID == "" {
		t.Fatalf("capability request = %#v", req)
	}
	if err := conn.WriteText(protocol.MustJSON(protocol.ResultResponse(req.ID, map[string]any{"rows": []any{}, "count": 0}))); err != nil {
		t.Fatalf("write CLI issued key response: %v", err)
	}
	body = <-done
	if !strings.Contains(string(body), `"count":0`) {
		t.Fatalf("allowed REST body = %s", string(body))
	}
}

func TestGatewayOpenAPIAndMCPFilteringWithManualAgent(t *testing.T) {
	secrets := newITSecrets(t)
	limitedKey := "limited-api-key"
	limitedHash, err := bcrypt.GenerateFromPassword([]byte(limitedKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := gateway.NewServer(gateway.Config{
		Addr:           addr,
		AgentPublicKey: secrets.AgentPublicKey,
		APIKeys: []gateway.APIKey{
			{Name: "all", KeyHash: secrets.APIKeyHash, Capabilities: []string{"*"}},
			{Name: "limited", KeyHash: string(limitedHash), Capabilities: []string{"echo_customer"}},
		},
		RateLimit:    gateway.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
		AgentTimeout: time.Second,
	}, io.Discard)
	go func() { _ = srv.ListenAndServeContext(ctx) }()
	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)

	conn := dialManualAgent(t, "ws://"+addr+"/ws/agent", secrets.PrivateKey)
	defer conn.Close()
	serveMetaOnce(t, conn, openAPIDocFor("echo_customer", "hidden_customer"))
	waitForHTTP(t, baseURL+"/openapi.json", limitedKey, http.StatusOK)

	openAPI := getWithAPIKey(t, baseURL+"/openapi.json", limitedKey)
	if !strings.Contains(string(openAPI), "echo_customer") || strings.Contains(string(openAPI), "hidden_customer") {
		t.Fatalf("filtered OpenAPI = %s", string(openAPI))
	}
	tools := postMCPPayload(t, baseURL, limitedKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(string(tools), "echo_customer") || strings.Contains(string(tools), "hidden_customer") {
		t.Fatalf("filtered tools/list = %s", string(tools))
	}
	allTools := postMCPPayload(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.Contains(string(allTools), "echo_customer") || !strings.Contains(string(allTools), "hidden_customer") {
		t.Fatalf("wildcard tools/list = %s", string(allTools))
	}
}

func TestGatewayWireFormatAndAgentErrorDetailAreHidden(t *testing.T) {
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &bytes.Buffer{}
	baseURL := startInternalGatewayWithLog(t, ctx, addr, secrets, time.Second, logs)

	conn := dialManualAgent(t, "ws://"+addr+"/ws/agent", secrets.PrivateKey)
	defer conn.Close()
	metaReq := serveMetaOnce(t, conn, openAPIDocFor("echo_customer"))
	if metaReq.Capability != "meta" || metaReq.ID == "" {
		t.Fatalf("meta request = %#v", metaReq)
	}
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

	done := make(chan []byte, 1)
	go func() {
		status, body := postCapability(t, baseURL, secrets.APIKey, "echo_customer", `{"id":7,"secret":"dont-log-me"}`)
		if status != http.StatusBadGateway {
			body = []byte("status=" + http.StatusText(status) + " body=" + string(body))
		}
		done <- body
	}()
	msg, err := conn.ReadText()
	if err != nil {
		t.Fatalf("read capability request: %v", err)
	}
	var req protocol.Request
	if err := json.Unmarshal(msg, &req); err != nil {
		t.Fatalf("decode capability request: %v; msg=%s", err, string(msg))
	}
	if req.ID == "" || req.Capability != "echo_customer" || req.Params["secret"] != "dont-log-me" {
		t.Fatalf("capability request = %#v", req)
	}
	if err := conn.WriteText(protocol.MustJSON(protocol.Response{
		ID: req.ID,
		Error: &protocol.Error{
			Code:   "AGENT_VALIDATION_FAILED",
			Detail: "database password is dont-log-me",
		},
	})); err != nil {
		t.Fatalf("write agent error: %v", err)
	}
	body := <-done
	requireAPIErrorCode(t, body, "AGENT_VALIDATION_FAILED")
	if strings.Contains(string(body), "dont-log-me") || strings.Contains(logs.String(), "dont-log-me") {
		t.Fatalf("agent detail or params leaked; body=%s logs=%s", string(body), logs.String())
	}

	mcpDone := make(chan []byte, 1)
	go func() {
		status, body := postMCPStatus(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo_customer","arguments":{"secret":"dont-log-me-too"}}}`)
		if status != http.StatusBadGateway {
			body = []byte("status=" + http.StatusText(status) + " body=" + string(body))
		}
		mcpDone <- body
	}()
	msg, err = conn.ReadText()
	if err != nil {
		t.Fatalf("read MCP capability request: %v", err)
	}
	if err := json.Unmarshal(msg, &req); err != nil {
		t.Fatalf("decode MCP capability request: %v; msg=%s", err, string(msg))
	}
	if req.ID == "" || req.Capability != "echo_customer" || req.Params["secret"] != "dont-log-me-too" {
		t.Fatalf("MCP capability request = %#v", req)
	}
	if err := conn.WriteText(protocol.MustJSON(protocol.Response{
		ID: req.ID,
		Error: &protocol.Error{
			Code:   "AGENT_VALIDATION_FAILED",
			Detail: "database password is dont-log-me-too",
		},
	})); err != nil {
		t.Fatalf("write MCP agent error: %v", err)
	}
	mcpBody := <-mcpDone
	requireAPIErrorCode(t, mcpBody, "AGENT_VALIDATION_FAILED")
	if strings.Contains(string(mcpBody), "dont-log-me-too") || strings.Contains(logs.String(), "dont-log-me-too") {
		t.Fatalf("MCP agent detail or params leaked; body=%s logs=%s", string(mcpBody), logs.String())
	}
}

func serveMetaOnce(t *testing.T, conn *ws.Conn, doc map[string]any) protocol.Request {
	t.Helper()
	msg, err := conn.ReadText()
	if err != nil {
		t.Fatalf("read meta request: %v", err)
	}
	var req protocol.Request
	if err := json.Unmarshal(msg, &req); err != nil {
		t.Fatalf("decode meta request: %v; msg=%s", err, string(msg))
	}
	if req.Capability != "meta" {
		t.Fatalf("capability = %q, want meta", req.Capability)
	}
	if err := conn.WriteText(protocol.MustJSON(protocol.ResultResponse(req.ID, map[string]any{"data": doc}))); err != nil {
		t.Fatalf("write meta response: %v", err)
	}
	return req
}

func openAPIDocFor(caps ...string) map[string]any {
	paths := map[string]any{}
	for _, cap := range caps {
		paths["/api/v1/capabilities/"+cap] = map[string]any{
			"post": map[string]any{
				"x-onprest-capability": cap,
				"description":          cap + " description",
				"requestBody": map[string]any{
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"params": map[string]any{
										"type":       "object",
										"properties": map[string]any{"id": map[string]any{"type": "integer"}},
									},
								},
							},
						},
					},
				},
			},
		}
	}
	return map[string]any{"openapi": "3.1.0", "info": map[string]any{"title": "IT", "version": "0.1.0"}, "paths": paths}
}

func getWithAPIKey(t *testing.T, url, apiKey string) []byte {
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", url, resp.StatusCode, string(body))
	}
	return body
}

func postMCPPayload(t *testing.T, baseURL, apiKey, payload string) []byte {
	t.Helper()
	status, body := postMCPStatus(t, baseURL, apiKey, payload)
	if status != http.StatusOK {
		t.Fatalf("MCP status=%d body=%s", status, string(body))
	}
	return body
}

func postMCPStatus(t *testing.T, baseURL, apiKey, payload string) (int, []byte) {
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
	return resp.StatusCode, body
}

func quoteJSON(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func decodePrivateKey(t *testing.T, raw string) ed25519.PrivateKey {
	t.Helper()
	key, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decode private key: %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Fatalf("private key size = %d, want %d", len(key), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(key)
}
