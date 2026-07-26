package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

func fetchAgentChallenge(ctx context.Context, gatewayURL string) (string, error) {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("unsupported gateway URL scheme %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/challenge"
	u.RawQuery = ""
	u.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("agent challenge failed: %s", resp.Status)
	}
	var body struct {
		Challenge string `json:"challenge"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode agent challenge: %w", err)
	}
	if body.Challenge == "" {
		return "", fmt.Errorf("gateway returned an empty agent challenge")
	}
	return body.Challenge, nil
}

func setAgentAuthHeaders(headers http.Header, privateKeyRaw, path, challenge string) error {
	privateKeyBytes, err := base64.RawURLEncoding.DecodeString(privateKeyRaw)
	if err != nil {
		return fmt.Errorf("decode agent private key: %w", err)
	}
	if len(privateKeyBytes) != ed25519.PrivateKeySize {
		return fmt.Errorf("agent private key must be %d bytes", ed25519.PrivateKeySize)
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	timestamp := time.Now().UTC().Format(time.RFC3339)
	handshakeKey, err := ws.NewHandshakeKey()
	if err != nil {
		return err
	}
	if challenge == "" {
		return fmt.Errorf("agent challenge is empty")
	}
	message := protocol.AgentAuthMessage(path, timestamp, nonce, challenge, handshakeKey)
	signature := ed25519.Sign(ed25519.PrivateKey(privateKeyBytes), message)
	headers.Set("Sec-WebSocket-Key", handshakeKey)
	headers.Set("X-Agent-Timestamp", timestamp)
	headers.Set("X-Agent-Nonce", nonce)
	headers.Set("X-Agent-Challenge", challenge)
	headers.Set("X-Agent-Signature", base64.RawURLEncoding.EncodeToString(signature))
	return nil
}
