package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

func setAgentAuthHeaders(headers http.Header, privateKeyRaw, path string) error {
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
	message := protocol.AgentAuthMessage(path, timestamp, nonce, handshakeKey)
	signature := ed25519.Sign(ed25519.PrivateKey(privateKeyBytes), message)
	headers.Set("Sec-WebSocket-Key", handshakeKey)
	headers.Set("X-Agent-Timestamp", timestamp)
	headers.Set("X-Agent-Nonce", nonce)
	headers.Set("X-Agent-Signature", base64.RawURLEncoding.EncodeToString(signature))
	return nil
}
