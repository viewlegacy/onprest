package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
	"golang.org/x/crypto/bcrypt"
)

const (
	maxAPIKeys                         = 100
	maxAPIKeyCacheEntries              = 1024
	apiKeyCacheTTL                     = 5 * time.Minute
	maxConcurrentAPIKeyAuthentications = 4
	maxAgentChallenges                 = 1024
	agentChallengeTTL                  = time.Minute
)

func (s *Server) handleAgentChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError(errGatewayMethodNotAllowed, "use POST"))
		return
	}
	if !emptyAgentAuthBody(w, r) {
		return
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError(errGatewayInternal, "unexpected gateway error"))
		return
	}
	challenge := base64.RawURLEncoding.EncodeToString(raw[:])
	now := time.Now()
	s.authMu.Lock()
	s.cleanupAgentAuthStateLocked(now)
	if len(s.agentChallenges) >= maxAgentChallenges {
		s.evictOldestChallengeLocked()
	}
	s.agentChallenges[challenge] = now
	s.authMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]string{"challenge": challenge})
}

func (s *Server) handleAgentVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError(errGatewayMethodNotAllowed, "use POST"))
		return
	}
	if !emptyAgentAuthBody(w, r) {
		return
	}
	if !s.authenticateAgentVerify(r) {
		writeJSON(w, http.StatusUnauthorized, apiError(errGatewayAuthFailed, "invalid agent signature"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func emptyAgentAuthBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody {
		if r.ContentLength > 0 {
			writeJSON(w, http.StatusBadRequest, apiError(errGatewayInvalidRequest, "request body must be empty"))
			return false
		}
		return true
	}
	if r.ContentLength > 0 {
		writeJSON(w, http.StatusBadRequest, apiError(errGatewayInvalidRequest, "request body must be empty"))
		return false
	}
	body := http.MaxBytesReader(w, r.Body, 1)
	var one [1]byte
	n, err := body.Read(one[:])
	if n != 0 || (err != nil && err != io.EOF) {
		writeJSON(w, http.StatusBadRequest, apiError(errGatewayInvalidRequest, "request body must be empty"))
		return false
	}
	if err != io.EOF {
		writeJSON(w, http.StatusBadRequest, apiError(errGatewayInvalidRequest, "request body must be empty"))
		return false
	}
	return true
}

func (s *Server) authenticateAgent(r *http.Request) bool {
	publicKey, err := base64.RawURLEncoding.DecodeString(s.cfg.AgentPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return false
	}
	timestamp := r.Header.Get("X-Agent-Timestamp")
	nonce := r.Header.Get("X-Agent-Nonce")
	signatureRaw := r.Header.Get("X-Agent-Signature")
	challenge := r.Header.Get("X-Agent-Challenge")
	handshakeKey := r.Header.Get("Sec-WebSocket-Key")
	if timestamp == "" || nonce == "" || challenge == "" || signatureRaw == "" || handshakeKey == "" {
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
	if !ed25519.Verify(ed25519.PublicKey(publicKey), protocol.AgentAuthMessage(r.URL.Path, timestamp, nonce, challenge, handshakeKey), signature) {
		return false
	}
	return s.takeAgentCredentials(challenge, nonce, ts)
}

func (s *Server) authenticateAgentVerify(r *http.Request) bool {
	publicKey, err := base64.RawURLEncoding.DecodeString(s.cfg.AgentPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return false
	}
	timestamp := r.Header.Get("X-Agent-Timestamp")
	nonce := r.Header.Get("X-Agent-Nonce")
	signatureRaw := r.Header.Get("X-Agent-Signature")
	challenge := r.Header.Get("X-Agent-Challenge")
	if timestamp == "" || nonce == "" || challenge == "" || signatureRaw == "" || r.Header.Get("Sec-WebSocket-Key") != "" {
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
	if r.URL.Path != "/ws/agent/verify" || !ed25519.Verify(ed25519.PublicKey(publicKey), protocol.AgentVerifyAuthMessage(r.URL.Path, timestamp, nonce, challenge), signature) {
		return false
	}
	return s.takeAgentCredentials(challenge, nonce, ts)
}

func (s *Server) takeAgentCredentials(challenge, nonce string, ts time.Time) bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	now := time.Now()
	s.cleanupAgentAuthStateLocked(now)
	issued, ok := s.agentChallenges[challenge]
	if !ok || now.Sub(issued) > agentChallengeTTL {
		return false
	}
	if _, ok := s.nonces[nonce]; ok {
		return false
	}
	delete(s.agentChallenges, challenge)
	s.nonces[nonce] = ts
	return true
}

func (s *Server) cleanupAgentAuthStateLocked(now time.Time) {
	cutoff := now.Add(-5 * time.Minute)
	for k, seen := range s.nonces {
		if seen.Before(cutoff) {
			delete(s.nonces, k)
		}
	}
	challengeCutoff := now.Add(-agentChallengeTTL)
	for challenge, issued := range s.agentChallenges {
		if issued.Before(challengeCutoff) {
			delete(s.agentChallenges, challenge)
		}
	}
}

func (s *Server) evictOldestChallengeLocked() {
	var oldestChallenge string
	var oldest time.Time
	for challenge, issued := range s.agentChallenges {
		if oldest.IsZero() || issued.Before(oldest) {
			oldestChallenge, oldest = challenge, issued
		}
	}
	delete(s.agentChallenges, oldestChallenge)
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (APIKey, int) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.Header.Get("X-API-Key")
	}
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, apiError(errGatewayAuthFailed, "invalid api key"))
		return APIKey{}, http.StatusUnauthorized
	}
	digest := sha256.Sum256([]byte(token))
	if key, ok := s.cachedAPIKey(digest); ok {
		return key, 0
	}
	select {
	case s.apiKeyAuthSlots <- struct{}{}:
		defer func() { <-s.apiKeyAuthSlots }()
	default:
		writeJSON(w, http.StatusTooManyRequests, apiError(errGatewayRateLimited, "authentication is busy; retry later"))
		return APIKey{}, http.StatusTooManyRequests
	}
	// Recheck after acquiring a slot because another request may have populated
	// the success cache while this request was waiting.
	if key, ok := s.cachedAPIKey(digest); ok {
		return key, 0
	}
	for _, key := range s.cfg.APIKeys {
		if bcrypt.CompareHashAndPassword([]byte(key.KeyHash), []byte(token)) == nil {
			s.cacheAPIKey(digest, key)
			return key, 0
		}
	}
	writeJSON(w, http.StatusUnauthorized, apiError(errGatewayAuthFailed, "invalid api key"))
	return APIKey{}, http.StatusUnauthorized
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
