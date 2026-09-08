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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
	"github.com/viewlegacy/onprest/internal/gateway"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
	"golang.org/x/crypto/bcrypt"
)

func TestGeneratedIntegrationCapabilityFixturesStillLoad(t *testing.T) {
	secrets := newITSecrets(t)
	for _, driver := range []string{"postgres", "mysql", "sqlserver", "oracle"} {
		t.Run(driver, func(t *testing.T) {
			path := writeContainerCapability(t, t.TempDir(), driver, postgresConfig{
				Host: "127.0.0.1", Port: "1234", Name: "fixture", User: "fixture", Password: "fixture",
			}, "ws://127.0.0.1:8080/ws/agent", secrets.AgentPrivateKey, "select :id as id, 'name' as name, 'email' as email")
			if _, err := agentpkg.LoadCapabilityFile(path); err != nil {
				t.Fatalf("load generated %s integration fixture: %v", driver, err)
			}
		})
	}
}

type itSecrets struct {
	AgentPublicKey  string
	AgentPrivateKey string
	PrivateKey      ed25519.PrivateKey
	APIKey          string
	APIKeyHash      string
	APIKeysJSON     string
}

const validMCPInitializePayload = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"onprest-it","version":"test"}}}`

const modernMCPProtocolVersion = "2025-11-25"

type mcpToolCallResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   json.RawMessage `json:"error"`
	Result  struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	} `json:"result"`
}

type mcpRowsResult struct {
	Rows []struct {
		ID    json.Number `json:"id"`
		Name  string      `json:"name"`
		Email string      `json:"email"`
	} `json:"rows"`
	Count json.Number `json:"count"`
}

func requireMCPToolCallResponse(t *testing.T, body []byte, wantID string, wantStructured bool) mcpToolCallResponse {
	t.Helper()
	var response mcpToolCallResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode MCP tools/call response: %v; body=%s", err, body)
	}
	if response.JSONRPC != "2.0" || (len(response.Error) != 0 && string(response.Error) != "null") {
		t.Fatalf("MCP tools/call envelope=%#v; body=%s", response, body)
	}
	if string(response.ID) != wantID {
		t.Fatalf("MCP tools/call id=%s want %s; body=%s", response.ID, wantID, body)
	}
	if response.Result.IsError || len(response.Result.Content) != 1 || response.Result.Content[0].Type != "text" || response.Result.Content[0].Text == "" {
		t.Fatalf("MCP tools/call result=%#v, want successful one-text result; body=%s", response.Result, body)
	}
	if err := validateMCPStructuredContent(response.Result.StructuredContent, wantStructured); err != nil {
		t.Fatalf("MCP tools/call structuredContent: %v; body=%s", err, body)
	}
	return response
}

func validateMCPStructuredContent(raw json.RawMessage, wantStructured bool) error {
	if !wantStructured {
		if len(raw) != 0 {
			return fmt.Errorf("legacy response must omit structuredContent, got %s", raw)
		}
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("modern response is missing structuredContent")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return fmt.Errorf("modern structuredContent is not an object: %w", err)
	}
	if object == nil {
		return fmt.Errorf("modern structuredContent must be an object, got %s", raw)
	}
	return nil
}

func TestMCPStructuredContentPresenceValidation(t *testing.T) {
	for _, tc := range []struct {
		name           string
		raw            json.RawMessage
		wantStructured bool
		wantErr        bool
	}{
		{name: "legacy absent", wantErr: false},
		{name: "legacy null", raw: json.RawMessage(`null`), wantErr: true},
		{name: "legacy object", raw: json.RawMessage(`{"rows":[]}`), wantErr: true},
		{name: "modern absent", wantStructured: true, wantErr: true},
		{name: "modern null", raw: json.RawMessage(`null`), wantStructured: true, wantErr: true},
		{name: "modern object", raw: json.RawMessage(`{"rows":[]}`), wantStructured: true, wantErr: false},
		{name: "modern array", raw: json.RawMessage(`[]`), wantStructured: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateMCPStructuredContent(tc.raw, tc.wantStructured); (err != nil) != tc.wantErr {
				t.Fatalf("validateMCPStructuredContent(%q, %t) error=%v wantErr=%t", tc.raw, tc.wantStructured, err, tc.wantErr)
			}
		})
	}
}

func decodeMCPJSON(t *testing.T, raw []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode MCP JSON value: %v; raw=%s", err, raw)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("MCP JSON value has trailing data: err=%v raw=%s", err, raw)
	}
}

func assertMCPInitializeResponse(t *testing.T, body []byte, wantVersion string) {
	t.Helper()
	var response struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode MCP initialize response: %v; body=%s", err, body)
	}
	if response.Error != nil || response.Result.ProtocolVersion != "2025-03-26" || response.Result.ServerInfo.Version != wantVersion {
		t.Fatalf("MCP initialize result=%#v error=%#v, want protocol 2025-03-26/version %q; body=%s", response.Result, response.Error, wantVersion, body)
	}
}

func newITSecrets(t *testing.T) itSecrets {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
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
	return itSecrets{
		AgentPublicKey:  base64.RawURLEncoding.EncodeToString(publicKey),
		AgentPrivateKey: base64.RawURLEncoding.EncodeToString(privateKey),
		PrivateKey:      privateKey,
		APIKey:          apiKey,
		APIKeyHash:      string(apiKeyHash),
		APIKeysJSON:     string(apiKeysJSON),
	}
}

func writePostgresCapability(t *testing.T, dir string, db postgresConfig, gatewayURL, agentPrivateKey, capabilities string) string {
	t.Helper()
	return writePostgresCapabilityWithRuntimeAndLogging(t, dir, db, gatewayURL, agentPrivateKey, 0, "10MB", 3, capabilities)
}

func writePostgresCapabilityWithLogging(t *testing.T, dir string, db postgresConfig, gatewayURL, agentPrivateKey, maxSize string, maxFiles int, capabilities string) string {
	t.Helper()
	return writePostgresCapabilityWithRuntimeAndLogging(t, dir, db, gatewayURL, agentPrivateKey, 0, maxSize, maxFiles, capabilities)
}

func writePostgresCapabilityWithRuntime(t *testing.T, dir string, db postgresConfig, gatewayURL, agentPrivateKey string, maxConcurrentRequests int, capabilities string) string {
	t.Helper()
	return writePostgresCapabilityWithRuntimeAndLogging(t, dir, db, gatewayURL, agentPrivateKey, maxConcurrentRequests, "10MB", 3, capabilities)
}

func writePostgresCapabilityWithRuntimeAndLogging(t *testing.T, dir string, db postgresConfig, gatewayURL, agentPrivateKey string, maxConcurrentRequests int, maxSize string, maxFiles int, capabilities string) string {
	t.Helper()
	runtime := ""
	if maxConcurrentRequests > 0 {
		runtime = fmt.Sprintf("runtime:\n  max_concurrent_requests: %d\n", maxConcurrentRequests)
	}
	insecureGatewayOverride := ""
	if strings.HasPrefix(gatewayURL, "ws://gateway:") {
		insecureGatewayOverride = "  allow_insecure_non_loopback_ws: true\n"
	}
	content := fmt.Sprintf(`service:
  title: Onprest PostgreSQL IT
  version: 0.1.0
%s
database:
  driver: postgres
  host: %s
  port: %s
  name: %s
  user: %s
  password: %s
gateway:
  url: %s
  agent_private_key: %s
%s
logging:
  max_size: %s
  max_files: %d
capabilities:
%s
`, runtime, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey), insecureGatewayOverride, maxSize, maxFiles, capabilities)
	path := filepath.Join(dir, "capability.postgres.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write capability: %v", err)
	}
	return path
}

func startInternalGateway(t *testing.T, ctx context.Context, addr string, secrets itSecrets, agentTimeout time.Duration) string {
	t.Helper()
	return startInternalGatewayWithLog(t, ctx, addr, secrets, agentTimeout, io.Discard)
}

func startInternalGatewayWithLog(t *testing.T, ctx context.Context, addr string, secrets itSecrets, agentTimeout time.Duration, logOut io.Writer) string {
	return startInternalGatewayWithConfig(t, ctx, addr, secrets, agentTimeout, logOut, nil)
}

func startInternalGatewayWithConfig(t *testing.T, ctx context.Context, addr string, secrets itSecrets, agentTimeout time.Duration, logOut io.Writer, configure func(*gateway.Config)) string {
	t.Helper()
	if agentTimeout == 0 {
		agentTimeout = 500 * time.Millisecond
	}
	cfg := gateway.Config{
		Addr:           addr,
		AgentPublicKey: secrets.AgentPublicKey,
		APIKeys: []gateway.APIKey{{
			Name:         "it",
			KeyHash:      secrets.APIKeyHash,
			Capabilities: []string{"*"},
		}},
		RateLimit:    gateway.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
		AgentTimeout: agentTimeout,
	}
	if configure != nil {
		configure(&cfg)
	}
	srv := gateway.NewServer(cfg, logOut)
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServeContext(ctx)
	}()
	t.Cleanup(func() {
		select {
		case err := <-errCh:
			if err != nil && ctx.Err() == nil {
				t.Logf("gateway stopped: %v", err)
			}
		default:
		}
	})
	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)
	return baseURL
}

func signedAgentHeaders(t *testing.T, gatewayURL string, privateKey ed25519.PrivateKey, path string) http.Header {
	t.Helper()
	var nonceBytes [16]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		t.Fatal(err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes[:])
	timestamp := time.Now().UTC().Format(time.RFC3339)
	challenge := fetchIntegrationAgentChallenge(t, gatewayURL)
	handshakeKey, err := ws.NewHandshakeKey()
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, protocol.AgentAuthMessage(path, timestamp, nonce, challenge, handshakeKey))
	headers := http.Header{}
	headers.Set("Sec-WebSocket-Key", handshakeKey)
	headers.Set("X-Agent-Timestamp", timestamp)
	headers.Set("X-Agent-Nonce", nonce)
	headers.Set("X-Agent-Challenge", challenge)
	headers.Set("X-Agent-Signature", base64.RawURLEncoding.EncodeToString(signature))
	return headers
}

func dialManualAgent(t *testing.T, gatewayURL string, privateKey ed25519.PrivateKey) *ws.Conn {
	t.Helper()
	conn, err := ws.Dial(2*time.Second, gatewayURL, signedAgentHeaders(t, gatewayURL, privateKey, "/ws/agent"))
	if err != nil {
		t.Fatalf("dial agent websocket: %v", err)
	}
	return conn
}

func fetchIntegrationAgentChallenge(t *testing.T, gatewayURL string) string {
	t.Helper()
	u, err := url.Parse(gatewayURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme == "ws" {
		u.Scheme = "http"
	} else {
		u.Scheme = "https"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/challenge"
	resp, err := http.Post(u.String(), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("agent challenge status=%d body=%s", resp.StatusCode, body)
	}
	var body struct {
		Challenge string `json:"challenge"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Challenge == "" {
		t.Fatalf("decode agent challenge: %v", err)
	}
	return body.Challenge
}

func postCapability(t *testing.T, baseURL, apiKey, name, payload string) (int, []byte) {
	t.Helper()
	status, body, err := postCapabilityRequest(baseURL, apiKey, name, payload)
	if err != nil {
		t.Fatal(err)
	}
	return status, body
}

func postCapabilityRequest(baseURL, apiKey, name, payload string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/capabilities/"+name, strings.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

func requireAPIErrorCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var got struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, string(body))
	}
	if got.Error.Code != want {
		t.Fatalf("error code = %q, want %q; body=%s", got.Error.Code, want, string(body))
	}
	if got.Error.Message == "" {
		t.Fatalf("error message is empty; body=%s", string(body))
	}
}

func waitForExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("process did not exit within %s", timeout)
	}
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
