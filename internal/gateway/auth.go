package gateway

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
	"golang.org/x/crypto/bcrypt"
)

const (
	maxAPIKeys            = 100
	maxAPIKeyCacheEntries = 1024
	apiKeyCacheTTL        = 5 * time.Minute
)

func (s *Server) authenticateAgent(r *http.Request) bool {
	publicKey, err := base64.RawURLEncoding.DecodeString(s.cfg.AgentPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return false
	}
	timestamp := r.Header.Get("X-Agent-Timestamp")
	nonce := r.Header.Get("X-Agent-Nonce")
	signatureRaw := r.Header.Get("X-Agent-Signature")
	handshakeKey := r.Header.Get("Sec-WebSocket-Key")
	if timestamp == "" || nonce == "" || signatureRaw == "" || handshakeKey == "" {
		return false
	}
	ts, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return false
	}
	if d := time.Since(ts); d < -time.Minute || d > 5*time.Minute {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureRaw)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return false
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), protocol.AgentAuthMessage(r.URL.Path, timestamp, nonce, handshakeKey), signature) {
		return false
	}
	return s.takeAgentNonce(nonce, ts)
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
	digest := sha256.Sum256([]byte(token))
	if key, ok := s.cachedAPIKey(digest); ok {
		return key, true
	}
	for _, key := range s.cfg.APIKeys {
		if bcrypt.CompareHashAndPassword([]byte(key.KeyHash), []byte(token)) == nil {
			s.cacheAPIKey(digest, key)
			return key, true
		}
	}
	writeJSON(w, http.StatusUnauthorized, apiError(errGatewayAuthFailed, "invalid api key"))
	return APIKey{}, false
}

func (s *Server) cachedAPIKey(digest [sha256.Size]byte) (APIKey, bool) {
	now := time.Now()
	s.authMu.Lock()
	defer s.authMu.Unlock()
	entry, ok := s.apiKeyCache[digest]
	if !ok || now.After(entry.expires) {
		delete(s.apiKeyCache, digest)
		return APIKey{}, false
	}
	entry.lastUsed = now
	s.apiKeyCache[digest] = entry
	return entry.key, true
}

func (s *Server) cacheAPIKey(digest [sha256.Size]byte, key APIKey) {
	now := time.Now()
	s.authMu.Lock()
	defer s.authMu.Unlock()
	for hash, entry := range s.apiKeyCache {
		if now.After(entry.expires) {
			delete(s.apiKeyCache, hash)
		}
	}
	if len(s.apiKeyCache) >= maxAPIKeyCacheEntries {
		var oldestHash [sha256.Size]byte
		var oldest time.Time
		for hash, entry := range s.apiKeyCache {
			if oldest.IsZero() || entry.lastUsed.Before(oldest) {
				oldestHash, oldest = hash, entry.lastUsed
			}
		}
		delete(s.apiKeyCache, oldestHash)
	}
	s.apiKeyCache[digest] = cachedAPIKey{key: key, expires: now.Add(apiKeyCacheTTL), lastUsed: now}
}

func allowed(key APIKey, cap string) bool {
	for _, v := range key.Capabilities {
		if v == "*" || v == cap {
			return true
		}
	}
	return false
}
