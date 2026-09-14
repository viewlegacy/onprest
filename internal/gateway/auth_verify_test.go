package gateway

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
)

func TestAgentVerifyEndpointAcceptsDedicatedSignatureAndConsumesCredentials(t *testing.T) {
	s := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
	connected := &agentConn{conn: silentAgentConn{}, pending: map[string]chan agentResponse{}}
	s.agent = connected
	req := signedAgentVerifyRequest(t, s, time.Now().UTC(), "verify-nonce", nil)
	rec := httptest.NewRecorder()

	s.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	s.agentMu.RLock()
	if s.agent != connected {
		t.Fatal("verify changed the live agent connection")
	}
	s.agentMu.RUnlock()
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if len(s.agentChallenges) != 0 {
		t.Fatalf("challenge was not consumed: %#v", s.agentChallenges)
	}
	if _, ok := s.nonces["verify-nonce"]; !ok {
		t.Fatal("nonce was not recorded")
	}
}

func TestReplayNonceDoesNotConsumeFreshChallengeForVerifyOrWebSocket(t *testing.T) {
	t.Run("verify", func(t *testing.T) {
		s := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
		challenge := "shared-verify-challenge"
		replayedNonce := "replayed-verify-nonce"
		privateKeyBytes, err := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		s.authMu.Lock()
		s.agentChallenges[challenge] = time.Now()
		s.nonces[replayedNonce] = time.Now()
		s.authMu.Unlock()
		makeRequest := func(nonce string) *http.Request {
			timestamp := time.Now().UTC().Format(time.RFC3339)
			signature := ed25519.Sign(ed25519.PrivateKey(privateKeyBytes), protocol.AgentVerifyAuthMessage("/ws/agent/verify", timestamp, nonce, challenge))
			req := httptest.NewRequest(http.MethodPost, "/ws/agent/verify", nil)
			req.Header.Set("X-Agent-Timestamp", timestamp)
			req.Header.Set("X-Agent-Nonce", nonce)
			req.Header.Set("X-Agent-Challenge", challenge)
			req.Header.Set("X-Agent-Signature", base64.RawURLEncoding.EncodeToString(signature))
			return req
		}
		rec := httptest.NewRecorder()
		s.httpSrv.Handler.ServeHTTP(rec, makeRequest(replayedNonce))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("replayed nonce status=%d body=%s", rec.Code, rec.Body.String())
		}
		s.authMu.Lock()
		_, retained := s.agentChallenges[challenge]
		s.authMu.Unlock()
		if !retained {
			t.Fatal("replayed nonce consumed the fresh verify challenge")
		}
		rec = httptest.NewRecorder()
		s.httpSrv.Handler.ServeHTTP(rec, makeRequest("fresh-verify-nonce"))
		if rec.Code != http.StatusOK || rec.Body.String() != "{\"ok\":true}\n" {
			t.Fatalf("fresh nonce status=%d body=%s", rec.Code, rec.Body.String())
		}
		s.authMu.Lock()
		_, retained = s.agentChallenges[challenge]
		_, recorded := s.nonces["fresh-verify-nonce"]
		s.authMu.Unlock()
		if retained || !recorded {
			t.Fatalf("challenge/nonce state after fresh verify: retained=%t recorded=%t", retained, recorded)
		}
	})

	t.Run("websocket", func(t *testing.T) {
		s := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
		challenge := "shared-websocket-challenge"
		replayedNonce := "replayed-websocket-nonce"
		privateKeyBytes, err := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		s.authMu.Lock()
		s.agentChallenges[challenge] = time.Now()
		s.nonces[replayedNonce] = time.Now()
		s.authMu.Unlock()
		makeRequest := func(nonce string) *http.Request {
			timestamp := time.Now().UTC().Format(time.RFC3339)
			signature := ed25519.Sign(ed25519.PrivateKey(privateKeyBytes), protocol.AgentAuthMessage("/ws/agent", timestamp, nonce, challenge, testHandshakeKey))
			req := httptest.NewRequest(http.MethodGet, "/ws/agent", nil)
			req.Header.Set("Sec-WebSocket-Key", testHandshakeKey)
			req.Header.Set("X-Agent-Timestamp", timestamp)
			req.Header.Set("X-Agent-Nonce", nonce)
			req.Header.Set("X-Agent-Challenge", challenge)
			req.Header.Set("X-Agent-Signature", base64.RawURLEncoding.EncodeToString(signature))
			return req
		}
		if s.authenticateAgent(makeRequest(replayedNonce)) {
			t.Fatal("replayed nonce authenticated the WebSocket")
		}
		s.authMu.Lock()
		_, retained := s.agentChallenges[challenge]
		s.authMu.Unlock()
		if !retained {
			t.Fatal("replayed nonce consumed the fresh WebSocket challenge")
		}
		if !s.authenticateAgent(makeRequest("fresh-websocket-nonce")) {
			t.Fatal("fresh nonce was rejected for the retained WebSocket challenge")
		}
		s.authMu.Lock()
		_, retained = s.agentChallenges[challenge]
		s.authMu.Unlock()
		if retained {
			t.Fatal("successful WebSocket authentication did not consume the challenge")
		}
	})
}

func TestAgentVerifyEndpointRejectsNormalWebSocketDomainAndReplay(t *testing.T) {
	s := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
	wrongDomain := signedAgentVerifyRequest(t, s, time.Now().UTC(), "wrong-domain", func(path, timestamp, nonce, challenge string) []byte {
		return agentAuthMessage(path, timestamp, nonce, challenge)
	})
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, wrongDomain)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong domain status=%d body=%s", rec.Code, rec.Body.String())
	}
	withHandshakeKey := signedAgentVerifyRequest(t, s, time.Now().UTC(), "handshake-key", nil)
	withHandshakeKey.Header.Set("Sec-WebSocket-Key", testHandshakeKey)
	rec = httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, withHandshakeKey)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("handshake key status=%d body=%s", rec.Code, rec.Body.String())
	}
	_, wrongKeyPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey := signedAgentVerifyRequestWithChallengeAndSigner(t, s, time.Now().UTC().Format(time.RFC3339), "wrong-key", "wrong-key-challenge", func(path, timestamp, nonce, challenge string) []byte {
		return ed25519.Sign(wrongKeyPrivate, protocol.AgentVerifyAuthMessage(path, timestamp, nonce, challenge))
	})
	rec = httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, wrongKey)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key status=%d body=%s", rec.Code, rec.Body.String())
	}

	valid := signedAgentVerifyRequest(t, s, time.Now().UTC(), "replay-verify", nil)
	rec = httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, valid)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid status=%d body=%s", rec.Code, rec.Body.String())
	}
	replayed := signedAgentVerifyRequestWithChallenge(t, s, valid.Header.Get("X-Agent-Timestamp"), "replay-verify", valid.Header.Get("X-Agent-Challenge"), nil)
	rec = httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, replayed)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("replay status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAgentVerifyEndpointRejectsExpiredChallengeMethodAndBody(t *testing.T) {
	s := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
	expired := signedAgentVerifyRequest(t, s, time.Now().UTC(), "expired-verify", nil)
	s.authMu.Lock()
	s.agentChallenges[expired.Header.Get("X-Agent-Challenge")] = time.Now().Add(-agentChallengeTTL - time.Second)
	s.authMu.Unlock()
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, expired)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired status=%d body=%s", rec.Code, rec.Body.String())
	}

	method := signedAgentVerifyRequest(t, s, time.Now().UTC(), "method-verify", nil)
	method.Method = http.MethodGet
	rec = httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, method)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status=%d body=%s", rec.Code, rec.Body.String())
	}

	body := signedAgentVerifyRequest(t, s, time.Now().UTC(), "body-verify", nil)
	body.Body = io.NopCloser(bytes.NewBufferString("unexpected"))
	body.ContentLength = int64(len("unexpected"))
	rec = httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("body status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func signedAgentVerifyRequest(t *testing.T, s *Server, ts time.Time, nonce string, signer func(string, string, string, string) []byte) *http.Request {
	challenge := "verify-challenge-" + nonce
	return signedAgentVerifyRequestWithChallengeAndSigner(t, s, ts.Format(time.RFC3339), nonce, challenge, signer)
}

func signedAgentVerifyRequestWithChallenge(t *testing.T, s *Server, timestamp, nonce, challenge string, signer func(string, string, string, string) []byte) *http.Request {
	return signedAgentVerifyRequestWithChallengeAndSigner(t, s, timestamp, nonce, challenge, signer)
}

func signedAgentVerifyRequestWithChallengeAndSigner(t *testing.T, s *Server, timestamp, nonce, challenge string, signer func(string, string, string, string) []byte) *http.Request {
	t.Helper()
	privateKeyBytes, err := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	s.authMu.Lock()
	s.agentChallenges[challenge] = time.Now()
	s.authMu.Unlock()
	if signer == nil {
		signer = protocol.AgentVerifyAuthMessage
	}
	signature := ed25519.Sign(ed25519.PrivateKey(privateKeyBytes), signer("/ws/agent/verify", timestamp, nonce, challenge))
	req := httptest.NewRequest(http.MethodPost, "/ws/agent/verify", nil)
	req.Header.Set("X-Agent-Timestamp", timestamp)
	req.Header.Set("X-Agent-Nonce", nonce)
	req.Header.Set("X-Agent-Challenge", challenge)
	req.Header.Set("X-Agent-Signature", base64.RawURLEncoding.EncodeToString(signature))
	return req
}
