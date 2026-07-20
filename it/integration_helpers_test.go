//go:build integration

package it

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/gateway"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
	"golang.org/x/crypto/bcrypt"
)

type itSecrets struct {
	AgentPublicKey  string
	AgentPrivateKey string
	PrivateKey      ed25519.PrivateKey
	APIKey          string
	APIKeyHash      string
	APIKeysJSON     string
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
	return writePostgresCapabilityWithLogging(t, dir, db, gatewayURL, agentPrivateKey, "10MB", 3, capabilities)
}

func writePostgresCapabilityWithLogging(t *testing.T, dir string, db postgresConfig, gatewayURL, agentPrivateKey, maxSize string, maxFiles int, capabilities string) string {
	t.Helper()
	content := fmt.Sprintf(`service:
  title: Onprest PostgreSQL IT
  version: 0.1.0
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
logging:
  max_size: %s
  max_files: %d
capabilities:
%s
`, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey), maxSize, maxFiles, capabilities)
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
	t.Helper()
	if agentTimeout == 0 {
		agentTimeout = 500 * time.Millisecond
	}
	srv := gateway.NewServer(gateway.Config{
		Addr:           addr,
		AgentPublicKey: secrets.AgentPublicKey,
		APIKeys: []gateway.APIKey{{
			Name:         "it",
			KeyHash:      secrets.APIKeyHash,
			Capabilities: []string{"*"},
		}},
		RateLimit:    gateway.RateLimitConfig{RequestsPerSecond: 100, Burst: 100},
		AgentTimeout: agentTimeout,
	}, logOut)
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

func signedAgentHeaders(t *testing.T, privateKey ed25519.PrivateKey, path string) http.Header {
	t.Helper()
	var nonceBytes [16]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		t.Fatal(err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes[:])
	timestamp := time.Now().UTC().Format(time.RFC3339)
	handshakeKey, err := ws.NewHandshakeKey()
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, protocol.AgentAuthMessage(path, timestamp, nonce, handshakeKey))
	headers := http.Header{}
	headers.Set("Sec-WebSocket-Key", handshakeKey)
	headers.Set("X-Agent-Timestamp", timestamp)
	headers.Set("X-Agent-Nonce", nonce)
	headers.Set("X-Agent-Signature", base64.RawURLEncoding.EncodeToString(signature))
	return headers
}

func dialManualAgent(t *testing.T, gatewayURL string, privateKey ed25519.PrivateKey) *ws.Conn {
	t.Helper()
	conn, err := ws.Dial(2*time.Second, gatewayURL, signedAgentHeaders(t, privateKey, "/ws/agent"))
	if err != nil {
		t.Fatalf("dial agent websocket: %v", err)
	}
	return conn
}

func postCapability(t *testing.T, baseURL, apiKey, name, payload string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/capabilities/"+name, strings.NewReader(payload))
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
