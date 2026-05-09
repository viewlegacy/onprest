package gateway

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) authenticateAgent(r *http.Request) bool {
	publicKey, err := base64.RawURLEncoding.DecodeString(s.cfg.AgentPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return false
	}
	timestamp := r.Header.Get("X-Agent-Timestamp")
	nonce := r.Header.Get("X-Agent-Nonce")
	signatureRaw := r.Header.Get("X-Agent-Signature")
	if timestamp == "" || nonce == "" || signatureRaw == "" {
		return false
	}
	ts, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return false
	}
	if d := time.Since(ts); d < -time.Minute || d > 5*time.Minute {
		return false
	}
	if !s.takeAgentNonce(nonce, ts) {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureRaw)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(publicKey), protocol.AgentAuthMessage(r.URL.Path, timestamp, nonce), signature)
}

func (s *Server) takeAgentNonce(nonce string, ts time.Time) bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	cutoff := time.Now().Add(-5 * time.Minute)
	for k, seen := range s.nonces {
		if seen.Before(cutoff) {
			delete(s.nonces, k)
		}
	}
	if _, ok := s.nonces[nonce]; ok {
		return false
	}
	s.nonces[nonce] = ts
	return true
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (APIKey, bool) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.Header.Get("X-API-Key")
	}
	for _, key := range s.cfg.APIKeys {
		if bcrypt.CompareHashAndPassword([]byte(key.KeyHash), []byte(token)) == nil {
			return key, true
		}
	}
	writeJSON(w, http.StatusUnauthorized, apiError(errGatewayAuthFailed, "invalid api key"))
	return APIKey{}, false
}

func allowed(key APIKey, cap string) bool {
	for _, v := range key.Capabilities {
		if v == "*" || v == cap {
			return true
		}
	}
	return false
}
