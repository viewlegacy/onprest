package gateway

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestCreateAgentSecretEmitsUsableEd25519Keys(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if handled := HandleCLI([]string{"create-agent-secret"}, &stdout, &stderr); !handled {
		t.Fatal("HandleCLI did not handle create-agent-secret")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}

	var out struct {
		PublicKey  string `json:"agent_public_key"`
		PrivateKey string `json:"agent_private_key"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode output: %v; output=%s", err, stdout.String())
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(out.PublicKey)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	privateKey, err := base64.RawURLEncoding.DecodeString(out.PrivateKey)
	if err != nil {
		t.Fatalf("decode private key: %v", err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		t.Fatalf("public key size = %d, want %d", len(publicKey), ed25519.PublicKeySize)
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		t.Fatalf("private key size = %d, want %d", len(privateKey), ed25519.PrivateKeySize)
	}
	msg := []byte("onprest-agent-v2 test")
	sig := ed25519.Sign(ed25519.PrivateKey(privateKey), msg)
	if !ed25519.Verify(ed25519.PublicKey(publicKey), msg, sig) {
		t.Fatal("generated public key did not verify signature from private key")
	}
}

func TestCreateAgentAliasEmitsAgentSecretShape(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if handled := HandleCLI([]string{"create-agent"}, &stdout, &stderr); !handled {
		t.Fatal("HandleCLI did not handle create-agent")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	var out map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode output: %v; output=%s", err, stdout.String())
	}
	if out["agent_public_key"] == "" || out["agent_private_key"] == "" {
		t.Fatalf("create-agent output missing keys: %#v", out)
	}
}

func TestCreateKeyEmitsUsableHashAndCapabilities(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if handled := HandleCLI([]string{"create-key", "--name", "partner-a", "--capabilities", "get_customers, get_orders"}, &stdout, &stderr); !handled {
		t.Fatal("HandleCLI did not handle create-key")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}

	var out struct {
		Name         string   `json:"name"`
		APIKey       string   `json:"api_key"`
		KeyHash      string   `json:"key_hash"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode output: %v; output=%s", err, stdout.String())
	}
	if out.Name != "partner-a" {
		t.Fatalf("name = %q, want partner-a", out.Name)
	}
	if out.APIKey == "" || out.KeyHash == "" {
		t.Fatalf("missing api key or hash: %#v", out)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(out.KeyHash), []byte(out.APIKey)); err != nil {
		t.Fatalf("key hash does not verify api key: %v", err)
	}
	if got := strings.Join(out.Capabilities, ","); got != "get_customers,get_orders" {
		t.Fatalf("capabilities = %q", got)
	}
}

func TestCreateKeyWildcardCapability(t *testing.T) {
	var stdout, stderr bytes.Buffer

	HandleCLI([]string{"create-key", "--name", "admin", "--capabilities", "*"}, &stdout, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	var out struct {
		Capabilities string `json:"capabilities"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode output: %v; output=%s", err, stdout.String())
	}
	if out.Capabilities != "*" {
		t.Fatalf("capabilities = %q, want *", out.Capabilities)
	}
}

func TestCreateKeyRequiresName(t *testing.T) {
	var stdout, stderr bytes.Buffer

	HandleCLI([]string{"create-key", "--capabilities", "get_customers"}, &stdout, &stderr)
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--name is required") {
		t.Fatalf("stderr = %q, want --name error", stderr.String())
	}
}
